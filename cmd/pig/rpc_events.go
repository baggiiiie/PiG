// Ports packages/coding-agent/src/modes/json-event.ts.
package main

import (
	"encoding/json"
	"fmt"
	"slices"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/ai"
)

// rpcAgentEvent converts internal events to Pi JSON/RPC shapes in source insertion order. Conversion errors are returned so writer paths can terminate loudly.
func rpcAgentEvent(event agent.AgentEvent) ([]any, error) {
	switch event := event.(type) {
	case agent.AgentStartEvent:
		return []any{rpcObject{{"type", "agent_start"}}}, nil
	case agent.AgentEndEvent:
		messages, err := rpcAgentMessages(event.Messages)
		if err != nil {
			return nil, err
		}
		return []any{rpcObject{{"type", "agent_end"}, {"messages", messages}, {"willRetry", event.WillRetry}}}, nil
	case agent.AgentSettledEvent:
		return []any{rpcObject{{"type", "agent_settled"}}}, nil
	case agent.BashExecutionUpdateEvent:
		var id rpcRequestID
		if event.ID != nil {
			id = rpcRequestID(*event.ID)
		}
		return []any{RPCBashExecutionUpdate{Type: "bash_execution_update", ID: id, Delta: event.Delta}}, nil
	case agent.QueueUpdateEvent:
		return []any{RPCQueueUpdateEvent{Type: "queue_update", Steering: event.Steering, FollowUp: event.FollowUp}}, nil
	case agent.SessionInfoChangedEvent:
		return []any{rpcSessionInfoChanged(event.Name)}, nil
	case agent.ThinkingLevelChangedEvent:
		return []any{RPCThinkingLevelChangedEvent{Type: "thinking_level_changed", Level: event.Level}}, nil
	case agent.CompactionStartEvent:
		return []any{rpcObject{{"type", "compaction_start"}, {"reason", event.Reason}}}, nil
	case agent.CompactionEndEvent:
		out := rpcObject{{"type", "compaction_end"}, {"reason", event.Reason}}
		if event.Summary != "" {
			result := rpcObject{{"summary", event.Summary}, {"firstKeptEntryId", event.FirstKeptEntryID}, {"tokensBefore", event.TokensBefore}, {"estimatedTokensAfter", event.EstimatedTokensAfter}}
			if event.Usage != nil {
				result = append(result, rpcField{"usage", rpcUsage(event.Usage)})
			}
			if details := rpcOptionalCompactionDetails(event.Details); details != nil {
				result = append(result, rpcField{"details", details})
			}
			out = append(out, rpcField{"result", result})
		}
		out = append(out, rpcField{"aborted", event.Aborted}, rpcField{"willRetry", event.WillRetry})
		if event.ErrorMessage != "" {
			out = append(out, rpcField{"errorMessage", event.ErrorMessage})
		}
		return []any{out}, nil
	case agent.AutoRetryStartEvent:
		return []any{rpcObject{{"type", "auto_retry_start"}, {"attempt", event.Attempt}, {"maxAttempts", event.MaxAttempts}, {"delayMs", event.DelayMs}, {"errorMessage", event.ErrorMessage}}}, nil
	case agent.AutoRetryEndEvent:
		out := rpcObject{{"type", "auto_retry_end"}, {"success", event.Success}, {"attempt", event.Attempt}}
		if event.FinalError != "" {
			out = append(out, rpcField{"finalError", event.FinalError})
		}
		return []any{out}, nil
	case agent.SummarizationRetryScheduledEvent:
		return []any{rpcObject{{"type", "summarization_retry_scheduled"}, {"attempt", event.Attempt}, {"maxAttempts", event.MaxAttempts}, {"delayMs", event.DelayMs}, {"errorMessage", event.ErrorMessage}}}, nil
	case agent.SummarizationRetryAttemptStartEvent:
		out := rpcObject{{"type", "summarization_retry_attempt_start"}, {"source", event.Source}}
		if event.Source == "compaction" {
			out = append(out, rpcField{"reason", event.Reason})
		}
		return []any{out}, nil
	case agent.SummarizationRetryFinishedEvent:
		return []any{rpcObject{{"type", "summarization_retry_finished"}}}, nil
	case agent.TurnStartEvent:
		return []any{rpcObject{{"type", "turn_start"}}}, nil
	case agent.TurnEndEvent:
		message, err := rpcAgentMessage(event.Message)
		if err != nil {
			return nil, err
		}
		toolResults, err := rpcToolResultMessages(event.ToolResults)
		if err != nil {
			return nil, err
		}
		return []any{rpcObject{{"type", "turn_end"}, {"message", message}, {"toolResults", toolResults}}}, nil
	case agent.MessageStartEvent:
		message, err := rpcAgentMessage(event.Message)
		if err != nil {
			return nil, err
		}
		return []any{rpcObject{{"type", "message_start"}, {"message", message}}}, nil
	case agent.MessageUpdateEvent:
		update, err := rpcMessageUpdate(event)
		if err != nil {
			return nil, err
		}
		return []any{update}, nil
	case agent.MessageEndEvent:
		message, err := rpcAgentMessage(event.Message)
		if err != nil {
			return nil, err
		}
		return []any{rpcObject{{"type", "message_end"}, {"message", message}}}, nil
	case agent.EntryAppendedEvent:
		return []any{rpcSessionEntryAppendedEvent{Type: "entry_appended", Entry: event.Entry}}, nil
	case agent.ToolExecutionStartEvent:
		return []any{rpcObject{{"type", "tool_execution_start"}, {"toolCallId", event.ToolCallID}, {"toolName", event.ToolName}, {"args", json.RawMessage(event.Args)}}}, nil
	case agent.ToolExecutionUpdateEvent:
		// Shell startup has neither text nor details (bash.ts:301-302). Output snapshots carry details even when a chunk decodes to empty text (bash.ts:270-276).
		var text *string
		if event.Content != "" || event.Details != nil {
			text = &event.Content
		}
		return []any{rpcObject{{"type", "tool_execution_update"}, {"toolCallId", event.ToolCallID}, {"toolName", event.ToolName}, {"args", json.RawMessage(event.Args)}, {"partialResult", rpcToolResultPayload(text, nil, event.Details)}}}, nil
	case agent.ToolExecutionEndEvent:
		content := event.Result.Content
		if content == nil {
			content = []ai.ToolResultMessageContent{}
		}
		result := rpcObject{{"content", content}}
		if event.Result.Details != nil {
			result = append(result, rpcField{"details", event.Result.Details})
		}
		return []any{rpcObject{{"type", "tool_execution_end"}, {"toolCallId", event.ToolCallID}, {"toolName", event.ToolName}, {"result", result}, {"isError", event.Result.IsError}}}, nil
	default:
		return nil, fmt.Errorf("unsupported agent event %T", event)
	}
}

// rpcSessionEntryAppendedEvent carries a Session entry appended outside the agent loop exactly as it was persisted.
type rpcSessionEntryAppendedEvent struct {
	Type  string          `json:"type"`
	Entry json.RawMessage `json:"entry"`
}

func rpcMessageUpdate(event agent.MessageUpdateEvent) (any, error) {
	if event.Message.Assistant == nil {
		return nil, fmt.Errorf("message_update message is not an assistant message")
	}
	if event.AssistantMessageEvent == nil {
		return nil, fmt.Errorf("message_update assistant event is nil")
	}
	encoded, err := json.Marshal(event.AssistantMessageEvent)
	if err != nil {
		return nil, fmt.Errorf("marshal message_update assistant event: %w", err)
	}
	var wire rpcObject
	if err := json.Unmarshal(encoded, &wire); err != nil {
		return nil, fmt.Errorf("decode message_update assistant event: %w", err)
	}
	wire = slices.DeleteFunc(wire, func(field rpcField) bool { return field.name == "partial" })
	if start, ok := event.AssistantMessageEvent.(ai.ToolCallStartEvent); ok {
		if start.Partial == nil || start.ContentIndex < 0 || start.ContentIndex >= len(start.Partial.Content) {
			return nil, fmt.Errorf("toolcall_start content index %d is invalid", start.ContentIndex)
		}
		call, ok := start.Partial.Content[start.ContentIndex].(ai.ToolCall)
		if !ok {
			return nil, fmt.Errorf("toolcall_start content at index %d is not a tool call", start.ContentIndex)
		}
		wire = append(wire, rpcField{"id", call.ID}, rpcField{"toolName", call.Name})
	}
	return rpcObject{{"type", "message_update"}, {"usage", rpcUsage(event.Message.Assistant.Usage)}, {"assistantMessageEvent", wire}}, nil
}

func rpcAgentMessages(messages []agent.AgentMessage) ([]any, error) {
	out := make([]any, 0, len(messages))
	for i, message := range messages {
		wire, err := rpcAgentMessage(message)
		if err != nil {
			return nil, fmt.Errorf("agent messages[%d]: %w", i, err)
		}
		out = append(out, wire)
	}
	return out, nil
}

func rpcAgentMessage(message agent.AgentMessage) (any, error) {
	switch {
	case message.System != nil:
		return message.System, nil
	case message.User != nil:
		content, err := rpcUserContent(message.User.Content)
		if err != nil {
			return nil, err
		}
		return rpcObject{{"role", "user"}, {"content", content}, {"timestamp", message.User.Timestamp}}, nil
	case message.Assistant != nil:
		content, err := rpcAssistantContent(message.Assistant.Content)
		if err != nil {
			return nil, err
		}
		assistant := message.Assistant
		out := rpcObject{{"role", "assistant"}, {"content", content}, {"api", assistant.API}, {"provider", assistant.Provider}, {"model", assistant.ModelID}, {"usage", rpcUsage(assistant.Usage)}}
		// JSON.stringify omits an undefined stopReason; timestamp is part of the initial provider message, before subsequently appended metadata.
		if assistant.StopReason != "" {
			out = append(out, rpcField{"stopReason", assistant.StopReason})
		}
		out = append(out, rpcField{"timestamp", assistant.Timestamp})
		if assistant.ResponseModel != "" {
			out = append(out, rpcField{"responseModel", assistant.ResponseModel})
		}
		if assistant.ResponseID != "" {
			out = append(out, rpcField{"responseId", assistant.ResponseID})
		}
		if assistant.ProviderThinkingLevel != "" {
			out = append(out, rpcField{"providerThinkingLevel", assistant.ProviderThinkingLevel})
		}
		if len(assistant.Diagnostics) > 0 {
			out = append(out, rpcField{"diagnostics", assistant.Diagnostics})
		}
		if assistant.Deferred != nil {
			out = append(out, rpcField{"deferred", assistant.Deferred})
		}
		if assistant.ErrorMessage != "" {
			out = append(out, rpcField{"errorMessage", assistant.ErrorMessage})
		}
		if assistant.RawStopReason != "" {
			out = append(out, rpcField{"rawStopReason", assistant.RawStopReason})
		}
		if assistant.EndTurn != nil {
			out = append(out, rpcField{"endTurn", *assistant.EndTurn})
		}
		return out, nil
	case message.ToolResult != nil:
		return rpcToolResultMessage(*message.ToolResult)
	case message.Custom != nil:
		return message.Custom, nil
	default:
		return nil, fmt.Errorf("agent message has no variant")
	}
}

func rpcToolResultMessages(messages []agent.ToolResultMessage) ([]any, error) {
	out := make([]any, 0, len(messages))
	for i, message := range messages {
		wire, err := rpcToolResultMessage(message)
		if err != nil {
			return nil, fmt.Errorf("tool results[%d]: %w", i, err)
		}
		out = append(out, wire)
	}
	return out, nil
}

func rpcToolResultMessage(message agent.ToolResultMessage) (any, error) {
	content, err := rpcToolResultContent(message.Content)
	if err != nil {
		return nil, err
	}
	out := rpcObject{{"role", "toolResult"}, {"toolCallId", message.ToolCallID}, {"toolName", message.ToolName}, {"content", content}}
	if message.Details != nil {
		out = append(out, rpcField{"details", message.Details})
	}
	if message.Usage != nil {
		out = append(out, rpcField{"usage", rpcUsage(message.Usage)})
	}
	out = append(out, rpcField{"isError", message.IsError}, rpcField{"timestamp", message.Timestamp})
	return out, nil
}

// rpcToolResultPayload preserves absent text separately from an explicit empty text block.
func rpcToolResultPayload(text *string, images []ai.ImageContent, details any) any {
	content := make([]any, 0, 1+len(images))
	if text != nil {
		content = append(content, rpcObject{{"type", "text"}, {"text", *text}})
	}
	for _, image := range images {
		content = append(content, rpcObject{{"type", "image"}, {"data", image.Data}, {"mimeType", image.MimeType}})
	}
	out := rpcObject{{"content", content}}
	if details != nil {
		out = append(out, rpcField{"details", details})
	}
	return out
}

func rpcUserContent(content ai.UserContent) (any, error) {
	if text, ok := content.(ai.UserText); ok {
		return string(text), nil
	}
	blocks, _ := content.(ai.UserContentBlocks)
	out := make([]any, 0, len(blocks))
	for i, block := range blocks {
		wire, err := rpcContentBlock(block)
		if err != nil {
			return nil, fmt.Errorf("user content[%d]: %w", i, err)
		}
		out = append(out, wire)
	}
	return out, nil
}

func rpcAssistantContent(blocks []ai.AssistantContentBlock) ([]any, error) {
	out := make([]any, 0, len(blocks))
	for i, block := range blocks {
		wire, err := rpcContentBlock(block)
		if err != nil {
			return nil, fmt.Errorf("assistant content[%d]: %w", i, err)
		}
		out = append(out, wire)
	}
	return out, nil
}

func rpcToolResultContent(blocks []ai.ToolResultMessageContent) ([]any, error) {
	out := make([]any, 0, len(blocks))
	for i, block := range blocks {
		wire, err := rpcContentBlock(block)
		if err != nil {
			return nil, fmt.Errorf("tool result content[%d]: %w", i, err)
		}
		out = append(out, wire)
	}
	return out, nil
}

func rpcContentBlock(block ai.ContentBlock) (any, error) {
	switch block := block.(type) {
	case ai.TextContent:
		out := rpcObject{{"type", "text"}, {"text", block.Text}}
		if block.TextSignature != "" {
			out = append(out, rpcField{"textSignature", block.TextSignature})
		}
		return out, nil
	case ai.ImageContent:
		return rpcObject{{"type", "image"}, {"data", block.Data}, {"mimeType", block.MimeType}}, nil
	case ai.ToolCall:
		out := rpcObject{{"type", "toolCall"}, {"id", block.ID}, {"name", block.Name}, {"arguments", block.Arguments}}
		if block.ThoughtSignature != "" {
			out = append(out, rpcField{"thoughtSignature", block.ThoughtSignature})
		}
		if block.Namespace != "" {
			out = append(out, rpcField{"namespace", block.Namespace})
		}
		return out, nil
	case ai.ThinkingContent:
		out := rpcObject{{"type", "thinking"}, {"thinking", block.Thinking}}
		if block.ThinkingSignature != "" {
			out = append(out, rpcField{"thinkingSignature", block.ThinkingSignature})
		}
		if block.Redacted {
			out = append(out, rpcField{"redacted", true})
		}
		return out, nil
	default:
		return nil, fmt.Errorf("unsupported content block %T", block)
	}
}

// RPCUsage preserves reported totals and the insertion order of streaming usage counters.
type RPCUsage struct {
	Input        int          `json:"input"`
	Output       int          `json:"output"`
	CacheRead    int          `json:"cacheRead"`
	CacheWrite   int          `json:"cacheWrite"`
	TotalTokens  int          `json:"totalTokens"`
	Cost         ai.UsageCost `json:"cost"`
	CacheWrite1h *int         `json:"cacheWrite1h,omitempty"`
	Reasoning    *int         `json:"reasoning,omitempty"`
}

func rpcUsage(usage *ai.Usage) *RPCUsage {
	if usage == nil {
		usage = &ai.Usage{}
	}
	value := &RPCUsage{
		Input: usage.Input, Output: usage.Output, CacheRead: usage.CacheRead,
		CacheWrite: usage.CacheWrite, TotalTokens: usage.TotalTokens, Cost: usage.Cost,
	}
	if usage.CacheWrite1h != nil {
		value.CacheWrite1h = new(*usage.CacheWrite1h)
	}
	if usage.Reasoning != nil {
		value.Reasoning = new(*usage.Reasoning)
	}
	return value
}
