//go:build parity

package runner

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// configureProviderFixture runs a real HTTP provider endpoint and gives each binary the same models.json contract in its isolated agent directory.
func configureProviderFixture(t *testing.T, sc *Scenario, env []string, cwd, temp string) []string {
	t.Helper()
	if !sc.OpenAIFixture {
		return env
	}
	server := httptest.NewServer(http.HandlerFunc(strictOpenAIResponse))
	t.Cleanup(server.Close)
	agent := resultIdentityRoots(cwd, temp, env)["agent"]
	if agent == "" {
		t.Fatal("provider fixture requires an isolated agent directory")
	}
	if err := os.MkdirAll(agent, 0o700); err != nil {
		t.Fatal(err)
	}
	models := map[string]any{"providers": map[string]any{"strict-wire": map[string]any{"api": "openai-completions", "baseUrl": server.URL + "/v1", "apiKey": "fixture-key", "models": []any{map[string]any{"id": "strict", "name": "strict", "reasoning": false, "input": []string{"text"}, "cost": map[string]int{"input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0}, "contextWindow": 128000, "maxTokens": 1000}}}}}
	data, err := json.Marshal(models)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agent, "models.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return env
}

func strictOpenAIResponse(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Messages) == 0 {
		http.Error(w, "invalid fixture request", http.StatusBadRequest)
		return
	}
	last := body.Messages[len(body.Messages)-1]
	var text string
	if json.Unmarshal(last.Content, &text) != nil {
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(last.Content, &blocks); err != nil {
			http.Error(w, "invalid fixture content", http.StatusBadRequest)
			return
		}
		var b strings.Builder
		for _, block := range blocks {
			b.WriteString(block.Text)
		}
		text = b.String()
	}
	if text == "HTTP_ERROR" {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprint(w, `{"error":{"message":"strict wire bad request","type":"invalid_request_error"}}`)
		return
	}
	delta := map[string]any{"content": text}
	finish := "stop"
	if last.Role == "user" && text == "READ" {
		delta = map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "strict-read", "type": "function", "function": map[string]any{"name": "read", "arguments": `{"path":"parity-read-target.txt"}`}}}}
		finish = "tool_calls"
	}
	chunk := map[string]any{"id": "chatcmpl-strict", "model": "strict", "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}, "usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 3, "total_tokens": 13}}
	data, err := json.Marshal(chunk)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("content-type", "text/event-stream")
	_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", data)
}
