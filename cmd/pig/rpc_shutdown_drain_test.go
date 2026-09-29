package main

import (
	"reflect"
	"slices"
	"testing"
)

// Pi flushRawStdout (output-guard.ts:95-108) flushes writes and immediately fulfilled continuations, not future timers in post-disposal turn_end callbacks.
func TestRPCInputEndDoesNotJoinDelayedTurnEnd(t *testing.T) {
	for _, impl := range rpcShutdownImplementations {
		t.Run(impl.name, func(t *testing.T) {
			p, report := impl.start(t, "RPC_SHUTDOWN_TAIL_DELAY=1")
			afterEOF, events := endInputMidPrompt(p, report)
			wantEvents := rpcMidPromptInputEndExtensionEvents[:len(rpcMidPromptInputEndExtensionEvents)-1]
			if !reflect.DeepEqual(afterEOF, []rpcRecord{rpcShutdownStartedNotify}) || !slices.Equal(events, wantEvents) {
				t.Fatalf("shutdown joined a delayed boundary: stdout=%v events=%v; want only shutdown, events=%v", afterEOF, events, wantEvents)
			}
		})
	}
}

// Pi rpc-mode.ts:738 never resumes when a shutdown handler's Promise remains pending. Node exits on event-loop drain without aborting the tool or completing the handler.
func TestRPCInputEndMidToolDoesNotCompletePendingShutdown(t *testing.T) {
	for _, impl := range rpcShutdownImplementations {
		t.Run(impl.name, func(t *testing.T) {
			p, report := impl.start(t, "RPC_SHUTDOWN_BLOCK=1")
			afterEOF, events := endInputMidPrompt(p, report)
			wantEvents := rpcMidPromptInputEndExtensionEvents[:len(rpcMidPromptInputEndExtensionEvents)-2]
			if !reflect.DeepEqual(afterEOF, []rpcRecord{rpcShutdownStartedNotify}) || !slices.Equal(events, wantEvents) {
				t.Fatalf("pending shutdown resumed: stdout=%v events=%v; want only shutdown, events=%v", afterEOF, events, wantEvents)
			}
		})
	}
}
