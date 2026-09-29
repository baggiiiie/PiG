//go:build parity

package runner

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRPCSignalTeardownKeepsStdinOpen(t *testing.T) {
	sc := &Scenario{Name: "signal-open", RPC: RPCDriverConfig{Terminate: true, TimeoutSeconds: 5, Steps: []RPCInputStep{{Line: "request", WaitEvent: `{"type":"ready"}`, WaitTimeoutSeconds: 1}}}}
	bin := BinaryRef{Label: "pig", Path: "sh", Args: []string{"-c", `trap 'printf "signal\n" >&2; exit 143' TERM; read -r line; printf '{"type":"ready"}\n'; while read -r line; do :; done; printf 'EOF\n' >&2; exit 7`}}
	result := (rpcModeDriver{}).Run(t.Context(), t, bin, sc)
	if result.Err != nil || result.ExitCode != 143 || result.Stderr != "signal\n" || !result.StderrCaptured {
		t.Fatalf("signal teardown lost open stdin or stderr: %+v", result)
	}
}

func TestRPCEventBarrierRequiresOneNewCompleteCorrelatedRecord(t *testing.T) {
	for _, tc := range []struct {
		output string
		pass   bool
	}{
		{"{\"type\":\"response\",\"id\":\"old\"}\n", false},
		{"{\"type\":\"response\",\"id\":\"new\"}", false},
		{"{\"type\":\"response\",\"id\":\"new\"} garbage\n", false},
		{"{\"type\":\"response\",\"id\":\"old\"}\n{\"type\":\"other\",\"id\":\"new\"}\n", false},
		{"{\"id\":\"new\",\"type\":\"response\"}\n", true},
	} {
		var b synchronizedBuffer
		_, _ = b.Write([]byte(tc.output))
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		err := waitForRPCOutput(ctx, &b, 0, nil, `{"type":"response","id":"new"}`, 1)
		if (err == nil) != tc.pass {
			t.Fatalf("%q: %v", tc.output, err)
		}
	}
}

func TestProviderFixtureDrivesActualToolResultsAndHTTPFailure(t *testing.T) {
	for _, tc := range []struct {
		body     string
		status   int
		contains string
	}{
		{`{"messages":[{"role":"user","content":[{"type":"text","text":"READ"}]}]}`, 200, `"name":"read"`},
		{`{"messages":[{"role":"tool","content":"actual  result\n"}]}`, 200, `"content":"actual  result\n"`},
		{`{"messages":[{"role":"user","content":"HTTP_ERROR"}]}`, 400, `"message":"strict wire bad request"`},
	} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(tc.body))
		strictOpenAIResponse(w, r)
		if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.contains) {
			t.Fatalf("fixture output: %d %s", w.Code, w.Body.String())
		}
	}
}
