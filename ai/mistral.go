package ai

// Ports packages/ai/src/api/mistral-conversations.ts.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/MichaelKinsy/PiG/internal/jsstring"
	"github.com/MichaelKinsy/PiG/internal/nodeurl"
)

const (
	mistralToolCallIDLength  = 9
	maxMistralErrorBodyChars = 4000
	mistralJSWhitespace      = "\t\n\v\f\r \u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000\ufeff"
)

// MistralConfig configures the Mistral API provider.
type MistralConfig struct {
	// ModelMetadata retains the selected model's input capabilities, identity and thinking map.
	ModelMetadata *Model
	APIKey        string
	Model         string
	ProviderID    string
	BaseURL       string
	ExtraHeaders  map[string]string
	// SessionID for x-affinity header (KV-cache reuse).
	SessionID string
	// Reasoning indicates whether the model supports extended reasoning.
	Reasoning bool
}

type mistralProvider struct {
	cfg    MistralConfig
	client *http.Client
}

// NewMistralProvider creates a Provider backed by the Mistral API. BaseURL defaults to https://api.mistral.ai; requests resolve v1/chat/completions beneath its normalized base pathname, preserving any existing v1 segment.
func NewMistralProvider(cfg MistralConfig) Provider {
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://api.mistral.ai"
	}
	if cfg.ProviderID == "" {
		cfg.ProviderID = "mistral"
	}
	return &mistralProvider{cfg: cfg, client: streamingHTTPClientNoRetry()}
}

func (p *mistralProvider) ID() string   { return p.cfg.ProviderID }
func (p *mistralProvider) Close() error { return nil }

func mistralChatCompletionsURL(baseURL string) (string, error) {
	return nodeurl.ResolveDirectoryPath(baseURL, "v1/chat/completions")
}

// ─── Request wire types ──────────────────────────────────────────────────────

type mistralRequest struct {
	Model           string           `json:"model"`
	Messages        []mistralMessage `json:"messages"`
	Stream          bool             `json:"stream"`
	Tools           []mistralTool    `json:"tools,omitempty"`
	Temperature     *float64         `json:"temperature,omitempty"`
	MaxTokens       *int             `json:"maxTokens,omitempty"`
	ToolChoice      any              `json:"toolChoice,omitempty"`
	PromptMode      string           `json:"promptMode,omitempty"`
	ReasoningEffort string           `json:"reasoningEffort,omitempty"`
	PromptCacheKey  string           `json:"promptCacheKey,omitempty"`
}

type mistralMessage struct {
	Role       string `json:"role"`
	Prefix     *bool  `json:"prefix,omitempty"`
	Content    any    `json:"content,omitempty"`   // string | []mistralContentChunk
	ToolCalls  any    `json:"toolCalls,omitempty"` // []mistralToolCallMsg for assistant
	ToolCallID string `json:"toolCallId,omitempty"`
	Name       string `json:"name,omitempty"`
}

type mistralContentChunk struct {
	Type     string  `json:"type"`
	Text     *string `json:"text,omitempty"`
	ImageURL string  `json:"imageUrl,omitempty"`
	Thinking any     `json:"thinking,omitempty"` // []map[string]string for thinking content
}

type mistralToolCallMsg struct {
	ID       string               `json:"id"`
	Type     string               `json:"type"`
	Function mistralToolCallFnMsg `json:"function"`
	Index    int                  `json:"index"`
}

type mistralToolCallFnMsg struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type mistralTool struct {
	Type     string        `json:"type"`
	Function mistralToolFn `json:"function"`
}

type mistralToolFn struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
	Strict      bool           `json:"strict"`
}

// ─── Response wire types ─────────────────────────────────────────────────────

type mistralSSEChunk struct {
	ID      string             `json:"id"`
	Choices []mistralSSEChoice `json:"choices"`
	Usage   *mistralSSEUsage   `json:"usage,omitempty"`
}

type mistralSSEChoice struct {
	Delta        mistralSSEDelta `json:"delta"`
	FinishReason *string         `json:"finish_reason"`
}

type mistralSSEDelta struct {
	Content   json.RawMessage      `json:"content"` // string | []mistralContentItem | null
	ToolCalls []mistralSSEToolCall `json:"tool_calls"`
}

type mistralContentItem struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Thinking []struct {
		Text string `json:"text"`
	} `json:"thinking,omitempty"`
}

type mistralSSEToolCall struct {
	ID       string `json:"id"`
	Index    *int   `json:"index"`
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}

type mistralSSEUsage struct {
	PromptTokens             int                  `json:"prompt_tokens"`
	CompletionTokens         int                  `json:"completion_tokens"`
	TotalTokens              int                  `json:"total_tokens"`
	PromptTokensDetails      *mistralCacheDetails `json:"promptTokensDetails"`
	PromptTokensDetailsSnake *mistralCacheDetails `json:"prompt_tokens_details"`
	PromptTokenDetails       *mistralCacheDetails `json:"promptTokenDetails"`
	PromptTokenDetailsSnake  *mistralCacheDetails `json:"prompt_token_details"`
	NumCachedTokens          *int                 `json:"numCachedTokens"`
	NumCachedTokensSnake     *int                 `json:"num_cached_tokens"`
}

type mistralCacheDetails struct {
	CachedTokens      *int `json:"cachedTokens"`
	CachedTokensSnake *int `json:"cached_tokens"`
}

func (usage mistralSSEUsage) cachedPromptTokens() int {
	var candidates []*int
	for _, details := range []*mistralCacheDetails{usage.PromptTokensDetails, usage.PromptTokensDetailsSnake, usage.PromptTokenDetails, usage.PromptTokenDetailsSnake} {
		if details != nil {
			candidates = append(candidates, details.CachedTokens, details.CachedTokensSnake)
		}
	}
	candidates = append(candidates, usage.NumCachedTokens, usage.NumCachedTokensSnake)
	for _, candidate := range candidates {
		if candidate != nil {
			return min(usage.PromptTokens, max(0, *candidate))
		}
	}
	return 0
}

// ─── ID normalization ────────────────────────────────────────────────────────

func deriveMistralToolCallID(id string, attempt int) string {
	var normalized strings.Builder
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			normalized.WriteRune(r)
		}
	}
	n := normalized.String()
	if attempt == 0 && len(n) == mistralToolCallIDLength {
		return n
	}
	seedBase := n
	if seedBase == "" {
		seedBase = id
	}
	seed := seedBase
	if attempt > 0 {
		seed = fmt.Sprintf("%s:%d", seedBase, attempt)
	}
	h := shortHash32(seed)
	// Filter to alnum
	var out strings.Builder
	for _, r := range h {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			out.WriteRune(r)
			if out.Len() >= mistralToolCallIDLength {
				break
			}
		}
	}
	return out.String()
}

type mistralIDNormalizer struct {
	forward map[string]string
	reverse map[string]string
}

func newMistralIDNormalizer() *mistralIDNormalizer {
	return &mistralIDNormalizer{
		forward: make(map[string]string),
		reverse: make(map[string]string),
	}
}

func (n *mistralIDNormalizer) normalize(id string) string {
	if existing, ok := n.forward[id]; ok {
		return existing
	}
	for attempt := range 1000 {
		candidate := deriveMistralToolCallID(id, attempt)
		owner, taken := n.reverse[candidate]
		if !taken || owner == id {
			n.forward[id] = candidate
			n.reverse[candidate] = id
			return candidate
		}
		_ = attempt
	}
	// Fallback (should never happen)
	return id
}

// ─── Stream ──────────────────────────────────────────────────────────────────

func (p *mistralProvider) resolveModel() *Model {
	if p.cfg.ModelMetadata != nil {
		return new(*p.cfg.ModelMetadata)
	}
	if generated, ok := LookupModelExact(p.cfg.ProviderID + "/" + p.cfg.Model); ok {
		return generated.ToModel()
	}
	return &Model{
		ID: p.cfg.Model, Input: []string{"text"},
		ProviderMeta: ProviderMetadata{API: APIMistralConversations, ProviderID: p.cfg.ProviderID, BaseURL: p.cfg.BaseURL, Headers: p.cfg.ExtraHeaders, Reasoning: p.cfg.Reasoning},
		Capabilities: ModelCapabilities{MaxThinking: ThinkingHigh},
	}
}

func (p *mistralProvider) Stream(ctx context.Context, transcript TranscriptContext, opts StreamOptions) (*AssistantMessageEventStream, error) {
	if err := validateProviderRequest(ctx, transcript); err != nil {
		return nil, fmt.Errorf("mistral: invalid transcript: %w", err)
	}
	apiKey := p.cfg.APIKey
	if apiKey == "" {
		return nil, fmt.Errorf("no API key for Mistral provider")
	}

	model := p.resolveModel()
	supportsMidConversation := model.ProviderMeta.Compat != nil && model.ProviderMeta.Compat.SupportsMidConvoSystemMessages != nil && *model.ProviderMeta.Compat.SupportsMidConvoSystemMessages
	resolved := ResolveTranscript(transcript, supportsMidConversation)
	normalizer := newMistralIDNormalizer()
	messages := TransformMessages(resolved.Messages(), model, func(id string, _ *Model, _ AssistantMessage) string { return normalizer.normalize(id) })
	msgs := p.convertMessages(WithoutInitialSystemMessage(messages), slices.Contains(model.Input, "image"))

	if systemPrompt := GetCurrentSystemPrompt(messages[:min(1, len(messages))]); systemPrompt != "" {
		system := mistralMessage{Role: "system", Content: sanitizeSurrogates(systemPrompt)}
		msgs = append([]mistralMessage{system}, msgs...)
	}

	req := mistralRequest{
		Model:    p.cfg.Model,
		Stream:   true,
		Messages: msgs,
	}
	tools := GetCurrentTools(messages)
	if len(tools) > 0 {
		convertedTools, err := p.convertTools(tools)
		if err != nil {
			return nil, fmt.Errorf("mistral: convert tools: %w", err)
		}
		req.Tools = convertedTools
	}
	if opts.IsReasoning {
		reasoning := ClampThinkingLevel(model, opts.Thinking)
		if reasoning != "" && reasoning != ThinkingOff {
			if usesMistralPromptModeReasoning(p.cfg.Model, model.ProviderMeta.Reasoning) {
				req.PromptMode = "reasoning"
			}
			if usesMistralReasoningEffort(p.cfg.Model) {
				if mapped, ok := model.ThinkingLevelMap[reasoning]; ok && mapped != nil {
					req.ReasoningEffort = *mapped
				} else {
					req.ReasoningEffort = "high"
				}
			}
		}
	}
	if opts.TemperatureSet || opts.Temperature != 0 {
		req.Temperature = new(opts.Temperature)
	}
	if opts.MaxTokens > 0 {
		m := opts.MaxTokens
		req.MaxTokens = &m
	}
	if opts.PromptMode != "" {
		req.PromptMode = opts.PromptMode
	}
	if opts.ReasoningEffort != "" {
		req.ReasoningEffort = opts.ReasoningEffort
	}
	req.ToolChoice = opts.ToolChoice
	// Upstream shouldUsePromptCaching requires an explicit request session ID and retention other than none.
	if shouldUseMistralPromptCaching(opts.SessionID, opts.CacheRetention) {
		req.PromptCacheKey = opts.SessionID
	}

	payload := any(req)
	if opts.OnPayload != nil {
		next, err := opts.OnPayload(req, model)
		if err != nil {
			return nil, fmt.Errorf("mistral: onPayload: %w", err)
		}
		if next != nil {
			payload = next
		}
	}

	wire, err := toMistralWirePayload(payload)
	if err != nil {
		return nil, fmt.Errorf("mistral: convert wire payload: %w", err)
	}
	body, err := json.Marshal(wire)
	if err != nil {
		return nil, fmt.Errorf("mistral: marshal request: %w", err)
	}

	// upstream: packages/ai/src/api/mistral-conversations.ts:requestMistralStream
	timeoutMs := 60_000
	if opts.TimeoutMs != nil {
		timeoutMs = *opts.TimeoutMs
	}
	requestCtx, cancel := context.WithTimeoutCause(ctx, time.Duration(timeoutMs)*time.Millisecond, errors.New("The operation was aborted due to timeout"))
	streamOwnsRequest := false
	finishRequest := cancel
	defer func() {
		if !streamOwnsRequest {
			finishRequest()
		}
	}()
	endpoint, err := mistralChatCompletionsURL(p.cfg.BaseURL)
	if err != nil {
		return nil, err
	}
	requestURL, err := nodeurl.RequestURL(endpoint)
	if err != nil {
		return nil, err
	}
	if opts.Fetch == nil && requestURL.User != nil {
		return nil, fmt.Errorf("Request cannot be constructed from a URL that includes credentials: %s", endpoint)
	}
	httpReq, err := http.NewRequestWithContext(requestCtx, http.MethodPost, "", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("mistral: new request: %w", err)
	}
	httpReq.URL, httpReq.Host = requestURL, requestURL.Host
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("User-Agent", PiUserAgent())
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)

	for k, v := range p.cfg.ExtraHeaders {
		httpReq.Header.Set(k, v)
	}
	sessionID := opts.SessionID
	if sessionID == "" {
		sessionID = p.cfg.SessionID
	}
	_, hasExplicitAffinity := httpReq.Header["X-Affinity"]
	if shouldUseMistralPromptCaching(sessionID, opts.CacheRetention) && !hasExplicitAffinity {
		httpReq.Header.Set("x-affinity", sessionID)
	}
	applyProviderHeaders(httpReq, opts.Headers)

	resp, err := providerHTTPClient(p.client, opts.Fetch).Do(httpReq)
	if err != nil {
		if cause := context.Cause(requestCtx); cause != nil {
			err = cause
		}
		return nil, fmt.Errorf("mistral: request failed: %w", err)
	}

	// Custom fetch bodies need explicit cancellation even when they ignore the request context.
	closeBody := sync.OnceFunc(func() { _ = resp.Body.Close() })
	abortDone := make(chan struct{})
	stopAbort := context.AfterFunc(requestCtx, func() {
		defer close(abortDone)
		closeBody()
	})
	finishRequest = func() {
		if !stopAbort() {
			<-abortDone
		}
		closeBody()
		cancel()
	}
	responseBody := &mistralResponseBody{reader: resp.Body, closeBody: closeBody, request: requestCtx}
	if err := observeProviderResponse(requestCtx, opts, resp, model); err != nil {
		return nil, err
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		errBody, err := io.ReadAll(responseBody)
		if err != nil {
			return nil, err
		}
		errText := truncateMistralError(strings.Trim(string(errBody), mistralJSWhitespace))
		if errText == "" {
			_, errText, _ = strings.Cut(resp.Status, " ")
			if errText == "" {
				errText = fmt.Sprintf("Request failed with status %d", resp.StatusCode)
			}
		}
		return nil, fmt.Errorf("Mistral API error (%d): %s", resp.StatusCode, errText)
	}

	builder := newAssistantStreamBuilder(ctx, APIMistralConversations, p.cfg.ProviderID, p.cfg.Model)
	builder.modelCost = opts.ModelCost
	builder.start()
	streamOwnsRequest = true
	go func() {
		defer finishRequest()
		p.consumeStream(ctx, responseBody, builder)
	}()
	return builder.stream, nil
}

func shouldUseMistralPromptCaching(sessionID string, retention CacheRetention) bool {
	return retention != CacheRetentionNone && sessionID != ""
}

// toMistralWirePayload converts only documented SDK property positions, leaving authored tool schemas and arguments unchanged.
func toMistralWirePayload(payload any) (map[string]any, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, err
	}
	remap := func(object map[string]any, source, target string) {
		if value, exists := object[source]; exists {
			object[target] = value
			delete(object, source)
		}
	}
	for _, keys := range [][2]string{{"topP", "top_p"}, {"maxTokens", "max_tokens"}, {"randomSeed", "random_seed"}, {"responseFormat", "response_format"}, {"toolChoice", "tool_choice"}, {"presencePenalty", "presence_penalty"}, {"frequencyPenalty", "frequency_penalty"}, {"parallelToolCalls", "parallel_tool_calls"}, {"reasoningEffort", "reasoning_effort"}, {"promptMode", "prompt_mode"}, {"promptCacheKey", "prompt_cache_key"}, {"safePrompt", "safe_prompt"}} {
		remap(wire, keys[0], keys[1])
	}
	messages, ok := wire["messages"].([]any)
	if !ok {
		return nil, fmt.Errorf("messages must be an array")
	}
	for _, value := range messages {
		message, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("message must be an object")
		}
		remap(message, "toolCalls", "tool_calls")
		remap(message, "toolCallId", "tool_call_id")
		if content, ok := message["content"].([]any); ok {
			for _, value := range content {
				chunk, ok := value.(map[string]any)
				if !ok {
					continue
				}
				for _, keys := range [][2]string{{"imageUrl", "image_url"}, {"documentUrl", "document_url"}, {"documentName", "document_name"}, {"fileId", "file_id"}, {"referenceIds", "reference_ids"}, {"inputAudio", "input_audio"}} {
					remap(chunk, keys[0], keys[1])
				}
			}
		}
	}
	if format, ok := wire["response_format"].(map[string]any); ok {
		remap(format, "jsonSchema", "json_schema")
		if schema, ok := format["json_schema"].(map[string]any); ok {
			remap(schema, "schemaDefinition", "schema")
		}
	}
	return wire, nil
}

func truncateMistralError(text string) string {
	units := jsstring.ToUTF16(text)
	if len(units) <= maxMistralErrorBodyChars {
		return text
	}
	return fmt.Sprintf("%s... [truncated %d chars]", jsstring.FromUTF16(units[:maxMistralErrorBodyChars]), len(units)-maxMistralErrorBodyChars)
}

// mistralEventData applies Pi's trimStart per data line and final trim after shared SSE framing.
func mistralEventData(data string) string {
	if strings.ContainsRune(data, '\n') {
		lines := strings.Split(data, "\n")
		for i := range lines {
			lines[i] = strings.TrimLeft(lines[i], mistralJSWhitespace)
		}
		data = strings.Join(lines, "\n")
	}
	return strings.Trim(data, mistralJSWhitespace)
}

func (p *mistralProvider) consumeStream(ctx context.Context, body io.ReadCloser, builder *assistantStreamBuilder) {
	defer func() { _ = body.Close() }()

	decoder := newSSEDecoder(body)

	var usage *Usage
	stopReason := StopReasonStop
	stopErrorMessage := ""
	hasFinishReason := false
	type toolKey struct {
		index int
		id    string
	}
	toolBlocksByKey := make(map[toolKey]int)

	for decoder.Next() {
		if err := ctx.Err(); err != nil {
			builder.fail(StopReasonAborted, err)
			return
		}
		data := mistralEventData(decoder.Event().Data)
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			break
		}

		var chunk mistralSSEChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			builder.fail(StopReasonError, fmt.Errorf("mistral: invalid SSE JSON: %w", err))
			return
		}

		if chunk.Choices == nil {
			builder.fail(StopReasonError, errors.New("Invalid Mistral streaming event"))
			return
		}

		if chunk.ID != "" {
			builder.setResponseMetadata(chunk.ID, "", "", "", nil)
		}
		if chunk.Usage != nil {
			cached := chunk.Usage.cachedPromptTokens()
			total := chunk.Usage.TotalTokens
			if total == 0 {
				total = chunk.Usage.PromptTokens + chunk.Usage.CompletionTokens
			}
			usage = &Usage{
				Input:       max(0, chunk.Usage.PromptTokens-cached),
				Output:      chunk.Usage.CompletionTokens,
				CacheRead:   cached,
				TotalTokens: total,
			}
			builder.calculateCost(usage)
			builder.setUsage(usage)
		}

		if len(chunk.Choices) == 0 {
			continue
		}
		choice := chunk.Choices[0]
		delta := choice.Delta

		// Handle content
		if len(delta.Content) > 0 && string(delta.Content) != "null" {
			// Try as string first
			var textStr string
			if err := json.Unmarshal(delta.Content, &textStr); err == nil {
				if textStr != "" {
					builder.textDelta(sanitizeSurrogates(textStr))
				}
			} else {
				// Try as array of content items
				var items []mistralContentItem
				if err := json.Unmarshal(delta.Content, &items); err == nil {
					for _, item := range items {
						switch item.Type {
						case "text":
							if item.Text != "" {
								builder.textDelta(sanitizeSurrogates(item.Text))
							}
						case "thinking":
							var thinkText strings.Builder
							for _, part := range item.Thinking {
								if part.Text != "" {
									thinkText.WriteString(part.Text)
								}
							}
							if thinkText.Len() > 0 {
								builder.thinkingDelta(sanitizeSurrogates(thinkText.String()), false)
							}
						}
					}
				}
			}
		}

		// Handle tool calls
		for _, tc := range delta.ToolCalls {
			callID := tc.ID
			index := 0
			if tc.Index != nil {
				index = *tc.Index
			}
			if callID == "" || callID == "null" {
				callID = deriveMistralToolCallID(fmt.Sprintf("toolcall:%d", index), 0)
			}
			key := toolKey{id: callID}
			if tc.Index != nil {
				key = toolKey{index: index}
			}
			blockIndex, exists := toolBlocksByKey[key]
			callName := tc.Function.Name
			if exists {
				callID, callName = "", ""
			} else {
				blockIndex = len(toolBlocksByKey)
				toolBlocksByKey[key] = blockIndex
			}

			var argsDelta string
			if len(tc.Function.Arguments) > 0 {
				// Try as string first
				var s string
				if err := json.Unmarshal(tc.Function.Arguments, &s); err == nil {
					argsDelta = s
				} else {
					argsDelta = string(tc.Function.Arguments)
				}
			}

			builder.toolCallDelta(streamToolCallDelta{
				index: blockIndex, id: callID, name: callName, argumentsDelta: argsDelta,
			})
		}

		// Handle finish reason
		if choice.FinishReason != nil && *choice.FinishReason != "" {
			hasFinishReason = true
			builder.setResponseMetadata("", "", *choice.FinishReason, "", nil)
			stopReason, stopErrorMessage = mapMistralStopReason(*choice.FinishReason)
			if stopReason == StopReasonError {
				builder.fail(stopReason, errors.New(stopErrorMessage))
				return
			}
		}
	}

	if err := ctx.Err(); err != nil {
		builder.fail(StopReasonAborted, err)
		return
	}
	if err := decoder.Err(); err != nil {
		builder.fail(StopReasonError, err)
		return
	}
	if !hasFinishReason {
		builder.fail(StopReasonError, errors.New("Mistral stream ended without a finish reason"))
		return
	}
	builder.done(stopReason, usage, stopErrorMessage)
}

func mapMistralStopReason(reason string) (StopReason, string) {
	switch reason {
	case "stop":
		return StopReasonStop, ""
	case "length", "model_length":
		return StopReasonLength, ""
	case "tool_calls":
		return StopReasonToolUse, ""
	case "error":
		return StopReasonError, "Provider stopped with: error"
	default:
		return StopReasonError, "Provider stopped with: " + reason
	}
}

func usesMistralReasoningEffort(modelID string) bool {
	// upstream: packages/ai/src/api/mistral-conversations.ts:usesReasoningEffort
	return modelID == "mistral-small-2603" || modelID == "mistral-small-latest" || strings.HasPrefix(modelID, "mistral-medium-") || modelID == "zai-glm-5-2"
}

func usesMistralPromptModeReasoning(modelID string, reasoning bool) bool {
	return reasoning && !usesMistralReasoningEffort(modelID)
}

// ─── Message conversion ─────────────────────────────────────────────────────

func (p *mistralProvider) convertMessages(messages []Message, supportsImages bool) []mistralMessage {
	result := make([]mistralMessage, 0, len(messages))
	for _, message := range messages {
		switch message := message.(type) {
		case SystemMessage:
			if text := RenderSystemMessageUpdate(message); text != "" {
				result = append(result, mistralMessage{Role: "system", Content: sanitizeSurrogates(text)})
			}
		case UserMessage:
			if converted, ok := p.convertUserMessage(message); ok {
				result = append(result, converted)
			}
		case AssistantMessage:
			converted := p.convertAssistantMessage(message)
			if converted.Content != nil || converted.ToolCalls != nil {
				result = append(result, converted)
			}
		case ToolResultMessage:
			result = append(result, p.convertToolResultMessage(message, supportsImages))
		}
	}
	return result
}

func (p *mistralProvider) convertUserMessage(message UserMessage) (mistralMessage, bool) {
	switch content := message.Content.(type) {
	case UserText:
		return mistralMessage{Role: "user", Content: sanitizeSurrogates(string(content))}, true
	case UserContentBlocks:
		var chunks []mistralContentChunk
		for _, block := range content {
			switch block := block.(type) {
			case TextContent:
				chunks = append(chunks, mistralContentChunk{Type: "text", Text: new(sanitizeSurrogates(block.Text))})
			case ImageContent:
				chunks = append(chunks, mistralContentChunk{Type: "image_url", ImageURL: fmt.Sprintf("data:%s;base64,%s", block.MimeType, block.Data)})
			}
		}
		if len(chunks) > 0 {
			return mistralMessage{Role: "user", Content: chunks}, true
		}
	}
	return mistralMessage{}, false
}

func (p *mistralProvider) convertAssistantMessage(message AssistantMessage) mistralMessage {
	var content []mistralContentChunk
	var toolCalls []mistralToolCallMsg
	for _, block := range message.Content {
		switch block := block.(type) {
		case TextContent:
			if trimJSWhitespace(block.Text) != "" {
				content = append(content, mistralContentChunk{Type: "text", Text: new(sanitizeSurrogates(block.Text))})
			}
		case ThinkingContent:
			if trimJSWhitespace(block.Thinking) != "" {
				content = append(content, mistralContentChunk{Type: "thinking", Thinking: []map[string]string{{"type": "text", "text": sanitizeSurrogates(block.Thinking)}}})
			}
		case ToolCall:
			arguments, _ := json.Marshal(block.Arguments)
			if block.Arguments == nil {
				arguments = []byte("{}")
			}
			toolCalls = append(toolCalls, mistralToolCallMsg{
				ID: block.ID, Type: "function",
				Function: mistralToolCallFnMsg{Name: block.Name, Arguments: string(arguments)},
			})
		}
	}
	converted := mistralMessage{Role: "assistant", Prefix: new(false)}
	if len(content) > 0 {
		converted.Content = content
	}
	if len(toolCalls) > 0 {
		converted.ToolCalls = toolCalls
	}
	return converted
}

func (p *mistralProvider) convertToolResultMessage(message ToolResultMessage, supportsImages bool) mistralMessage {
	var textParts []string
	hasImages := false
	for _, block := range message.Content {
		switch block := block.(type) {
		case TextContent:
			textParts = append(textParts, sanitizeSurrogates(block.Text))
		case ImageContent:
			hasImages = true
		}
	}
	text := buildMistralToolResultText(strings.Join(textParts, "\n"), hasImages, supportsImages, message.IsError)
	content := []mistralContentChunk{{Type: "text", Text: new(text)}}
	if supportsImages {
		for _, block := range message.Content {
			if image, ok := block.(ImageContent); ok {
				content = append(content, mistralContentChunk{Type: "image_url", ImageURL: "data:" + image.MimeType + ";base64," + image.Data})
			}
		}
	}
	return mistralMessage{
		Role: "tool", Content: content,
		ToolCallID: message.ToolCallID, Name: message.ToolName,
	}
}

func buildMistralToolResultText(text string, hasImages, supportsImages, isError bool) string {
	text = trimJSWhitespace(text)
	prefix := ""
	if isError {
		prefix = "[tool error] "
	}
	if text != "" {
		if hasImages && !supportsImages {
			text += "\n[tool image omitted: model does not support images]"
		}
		return prefix + text
	}
	if hasImages {
		if supportsImages {
			return prefix + "(see attached image)"
		}
		return prefix + "(image omitted: model does not support images)"
	}
	return prefix + "(no tool output)"
}

func (p *mistralProvider) convertTools(tools []ToolSchema) ([]mistralTool, error) {
	result := make([]mistralTool, len(tools))
	for i, tool := range tools {
		strict, err := resolveJSONSchemaStrictSampling(tool, true)
		if err != nil {
			return nil, err
		}
		parameters, err := getJSONSchemaToolParameters(tool, strict)
		if err != nil {
			return nil, err
		}
		result[i] = mistralTool{
			Type: "function",
			Function: mistralToolFn{
				Name:        tool.Name,
				Description: tool.Description,
				Parameters:  parameters,
				Strict:      strict != nil && *strict,
			},
		}
	}
	return result, nil
}
