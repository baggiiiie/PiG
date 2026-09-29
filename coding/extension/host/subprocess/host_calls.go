package subprocess

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"

	"github.com/MichaelKinsy/PiG/extensions/sdk/json"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/MichaelKinsy/PiG/coding/extension"
)

// Upstream extensions call the host in process, so every synchronous host API
// applies in program order and every Promise-returning API starts in program
// order. The host keeps that contract for extension→host calls: calls apply in
// arrival order within one ordering lane, and a call whose upstream API returns
// a Promise starts in order but completes on its own goroutine, so a later call
// never waits for a user, a process, or a model.
//
// A lane is keyed by the call's parent request. Calls an extension makes while
// it handles a host request belong to that request's lane. The host may be
// waiting for that request inside an earlier call of another lane, for example
// a setting change that emits an event to the same extension, so lanes never
// wait for one another.

// startsAsync reports whether the upstream API behind method returns a
// Promise. Such a call starts in lane order and then runs on its own goroutine.
func startsAsync(method string) bool {
	switch method {
	case "ui.addAutocompleteProvider", "ui.autocomplete.invoke":
		// The public call remains synchronous in its SDK. A reverse factory/provider callback can reenter the host, so release its call lane at callback initiation.
		return true
	case "ui.select", "ui.confirm", "ui.input", "ui.editor", CallUICustom,
		"setModel", "registerProvider", "exec", "complete", "modelStream", CallProviderObject, "getModelAuth", "getProviderAuth", "refreshModelRegistry", "compact",
		"waitForIdle", "newSession", "fork", "navigateTree", "switchSession", "reload",
		CallOAuthOnPrompt, CallOAuthOnSelect, CallOAuthOnManualCodeInput:
		return true
	default:
		return false
	}
}

// callLanes runs extension→host calls in arrival order per lane. Each lane
// drains on a goroutine that exits when the lane is empty.
type callLanes struct {
	mu    sync.Mutex
	lanes map[string]*callLane
}

type callLane struct {
	queue []func()
}

func newCallLanes() *callLanes {
	return &callLanes{lanes: make(map[string]*callLane)}
}

// push queues run behind every earlier call of the same lane.
func (l *callLanes) push(lane string, run func()) {
	l.mu.Lock()
	current := l.lanes[lane]
	if current != nil {
		current.queue = append(current.queue, run)
		l.mu.Unlock()
		return
	}
	current = &callLane{queue: []func(){run}}
	l.lanes[lane] = current
	l.mu.Unlock()
	go l.drain(lane, current)
}

func (l *callLanes) drain(lane string, current *callLane) {
	for {
		l.mu.Lock()
		if len(current.queue) == 0 {
			delete(l.lanes, lane)
			l.mu.Unlock()
			return
		}
		run := current.queue[0]
		current.queue[0] = nil
		current.queue = current.queue[1:]
		l.mu.Unlock()
		run()
	}
}

// barrier returns once every call queued before it has run. A Promise-shaped
// call has run once it applied its synchronous part.
func (l *callLanes) barrier() {
	var reached sync.WaitGroup
	l.mu.Lock()
	for _, lane := range l.lanes {
		reached.Add(1)
		lane.queue = append(lane.queue, reached.Done)
	}
	l.mu.Unlock()
	reached.Wait()
}

// queueCall orders one extension→host call. A Promise-shaped call runs on its
// own goroutine, and the lane advances once the call has applied its
// synchronous part (it marks initiation) or has finished, whichever is first.
// The UI bridge marks initiation; a bare call handler has no initiation point,
// so its call finishes before the lane advances.
func (h *Host) queueCall(me *managedExt, lanes *callLanes, callID string, call *CallPayload) {
	lanes.push(call.ParentRequestID, func() {
		if !startsAsync(call.Method) {
			h.runCall(me, callID, call, nil)
			return
		}
		initiated := make(chan struct{})
		var once sync.Once
		mark := func() { once.Do(func() { close(initiated) }) }
		finished := make(chan struct{})
		go func() {
			defer close(finished)
			defer mark()
			h.runCall(me, callID, call, mark)
		}()
		select {
		case <-initiated:
		case <-finished:
		}
	})
}

// runCall executes one extension→host call and sends its result. mark, when
// non-nil, is the call's initiation mark.
func (h *Host) runCall(me *managedExt, callID string, call *CallPayload, mark func()) {
	callCtx, releaseCall := me.conn.hostCallContext(call.ParentRequestID, callID)
	if h.uiBridge != nil {
		defer h.uiBridge.releaseAutocompleteCall(me.conn, call)
	}
	defer releaseCall()
	if callCtx.Err() != nil || !h.acceptsNodeGeneration(me) {
		return
	}
	if mark != nil {
		callCtx = extension.WithCallInitiation(callCtx, mark)
	}
	if call.Method == "watchSessionLog" {
		// Keep session responses ahead of incremental state pushes.
		me.entryCursorMu.Lock()
		defer me.entryCursorMu.Unlock()
	}
	// An extension must never crash the host. Any nil-pointer or state
	// inconsistency (especially during /reload transitions) becomes an error
	// result for this call.
	defer func() {
		if r := recover(); r != nil {
			errMsg := fmt.Sprintf("panic in HandleCall for %s.%s: %v", me.config.Name, call.Method, r)
			fmt.Fprintf(os.Stderr, "%s\n", errMsg)
			if callID != "" {
				_ = me.conn.Send(&Envelope{
					Type:       MsgCallResult,
					ID:         callID,
					CallResult: &CallResultPayload{Error: &ErrorInfo{Code: "internal_panic", Message: errMsg}},
				})
			}
		}
	}()

	var result *CallResultPayload
	var err error
	switch {
	case call.Method == CallRegisterTool:
		result, err = h.handleToolRegistration(callCtx, me, call)
	case call.Method == "event.subscribe" || call.Method == "event.unsubscribe":
		result, err = h.handleEventSubscription(me, call)
	case call.Method == "provider.retain" || call.Method == "provider.release":
		result, err = h.handleProviderReference(me, call)
	case call.Method == CallProviderObject:
		result, err = h.handleProviderObject(callCtx, me, call)
	case call.Method == CallProviderPublish || call.Method == CallProviderCallback:
		result, err = h.handleProviderPublication(callCtx, me, call)
	case call.Method == "registerProvider" || call.Method == "unregisterProvider":
		result, err = h.handleProviderRegistrationCall(callCtx, me, call)
	case strings.HasPrefix(call.Method, "oauth.cb."):
		result, err = h.handleOAuthCallback(callCtx, me.config.Name, call)
	case h.uiBridge != nil:
		result, err = h.uiBridge.handleCall(callCtx, me.config.Name, me.conn, call)
	case h.onCall != nil:
		result, err = h.onCall(me.config.Name, call)
	}
	if (call.Method == "setModel" || takesInteractiveFocus(call.Method)) && err == nil {
		// Pi's model changes and dialogs expose current state before their Promise settles, including cancellation and dialog errors returned in the payload.
		err = h.pushStateTo(callCtx, me)
	}
	if callID == "" {
		return
	}
	if err != nil {
		_ = me.conn.Send(&Envelope{
			Type:       MsgCallResult,
			ID:         callID,
			CallResult: &CallResultPayload{Error: &ErrorInfo{Message: err.Error()}},
		})
		return
	}
	if result == nil {
		return
	}
	sendErr := me.conn.Send(&Envelope{Type: MsgCallResult, ID: callID, CallResult: result})
	if tooLarge, ok := errors.AsType[*FrameTooLargeError](sendErr); ok {
		// No compliant extension can read a frame above MaxFrameSize; return a
		// small error result instead of writing an undeliverable frame that
		// would silently kill the extension.
		fmt.Fprintf(os.Stderr, "extension %s call %s: result of %d bytes exceeds the %d byte IPC frame limit; returning an error\n",
			me.config.Name, call.Method, tooLarge.Size, tooLarge.Max)
		_ = me.conn.Send(&Envelope{
			Type: MsgCallResult,
			ID:   callID,
			CallResult: &CallResultPayload{Error: &ErrorInfo{
				Code:    "result_too_large",
				Message: fmt.Sprintf("result of %d bytes exceeds the %d byte IPC frame limit", tooLarge.Size, tooLarge.Max),
			}},
		})
	}
}

// handleProviderRegistrationCall applies a provider registration an extension
// makes after it loaded, through pi.registerProvider or
// ctx.modelRegistry.registerProvider, and the matching unregistrations.
// Upstream's bindCore makes both take effect immediately (runner.ts), in the
// one registry every extension shares.
func (h *Host) handleProviderRegistrationCall(ctx context.Context, me *managedExt, call *CallPayload) (*CallResultPayload, error) {
	var request struct {
		Name   string                     `json:"name"`
		Config json.RawMessage            `json:"config"`
		Native *NativeProviderDeclaration `json:"native"`
	}
	if err := json.Unmarshal(call.Args, &request); err != nil {
		return nil, fmt.Errorf("parse %s args: %w", call.Method, err)
	}
	if strings.TrimSpace(request.Name) == "" {
		return nil, errors.New("provider name must not be empty")
	}
	if call.Method == "unregisterProvider" {
		h.mu.Lock()
		released := h.retireNativeProviderLocked(request.Name)
		me.providerNames = slices.DeleteFunc(me.providerNames, func(name string) bool { return name == request.Name })
		oauth := slices.Contains(me.oauthProviderNames, request.Name)
		me.oauthProviderNames = slices.DeleteFunc(me.oauthProviderNames, func(name string) bool { return name == request.Name })
		h.mu.Unlock()
		h.releaseProviderCallbacks(released)
		h.providerRuntime.UnregisterProvider(request.Name)
		if oauth {
			ai.UnregisterOAuthProvider(request.Name)
		}
		if h.uiBridge != nil {
			h.uiBridge.ForgetProviderRegistration(request.Name)
		}
		return &CallResultPayload{}, nil
	}
	if request.Native != nil {
		if request.Native.ID != request.Name {
			return nil, errors.New("native provider id does not match registration")
		}
		if err := h.registerNativeProvider(ctx, me, request.Native); err != nil {
			return nil, err
		}
		return &CallResultPayload{}, nil
	}
	var config extension.ProviderConfig
	if err := json.Unmarshal(request.Config, &config); err != nil {
		return nil, fmt.Errorf("provider %s: decode config: %w", request.Name, err)
	}
	if err := h.providerRuntime.RegisterProvider(request.Name, config, extConfigOrigin(me.config)); err != nil {
		return nil, err
	}
	h.mu.Lock()
	released := h.retireNativeProviderLocked(request.Name)
	h.mu.Unlock()
	h.releaseProviderCallbacks(released)
	h.mu.Lock()
	if !slices.Contains(me.providerNames, request.Name) {
		me.providerNames = append(me.providerNames, request.Name)
	}
	registeredOAuth := slices.Contains(me.oauthProviderNames, request.Name)
	h.mu.Unlock()
	if !registeredOAuth {
		if err := h.registerOAuthProvider(me, request.Name, request.Config); err != nil {
			return nil, err
		}
	}
	if h.uiBridge != nil {
		h.uiBridge.RecordProviderRegistration(request.Name, request.Config)
	}
	return &CallResultPayload{}, nil
}
