package ai

// Ports packages/ai/src/api/lazy.ts
// Ports packages/ai/src/types.ts

import (
	"context"
	"fmt"
	"time"
)

// LazyAPICapabilities declares optional methods without loading the implementation.
type LazyAPICapabilities struct {
	FetchDeferred  bool
	CancelDeferred bool
}

// LazyStream returns before setup completes. The producer owns cancellation and its final event/result; forwarding never replaces an established terminal value with an iterator cancellation.
func LazyStream(ctx context.Context, model *Model, setup func(context.Context) (*AssistantMessageEventStream, error)) *AssistantMessageEventStream {
	outer := NewAssistantMessageEventStream()
	go func() {
		inner, err := setup(ctx)
		if err == nil && inner == nil {
			err = fmt.Errorf("API returned no event stream")
		}
		if err == nil {
			for event := range inner.Events(context.WithoutCancel(ctx)) {
				if err = outer.Push(event); err != nil {
					break
				}
			}
			if err == nil {
				result, resultErr := inner.ResultContext(context.WithoutCancel(ctx))
				if resultErr == nil {
					outer.End(result)
					return
				}
				err = resultErr
			}
		}
		message := &AssistantMessage{Content: []AssistantContentBlock{}, StopReason: StopReasonError, ErrorMessage: err.Error(), Timestamp: time.Now().UnixMilli()}
		if model != nil {
			message.API = model.ProviderMeta.API
			message.Provider = modelProviderID(model)
			message.Model = model.ID
		}
		_ = outer.Push(ErrorEvent{Reason: StopReasonError, Error: message})
	}()
	return outer
}
func (stream *AssistantMessageEventStream) isTerminated() bool {
	stream.mu.Lock()
	defer stream.mu.Unlock()
	return stream.terminal
}
func modelProviderID(model *Model) string {
	if model.ProviderMeta.ProviderID != "" {
		return model.ProviderMeta.ProviderID
	}
	if model.Provider != nil {
		return model.Provider.ID()
	}
	return ""
}

// LazyAPI invokes the loader per operation, as Pi does. Module caching belongs to the loader; unsupported deferred callbacks stay nil.
func LazyAPI(load func(context.Context) (*ProviderStreams, error), capabilities LazyAPICapabilities) *ProviderStreams {
	wrap := func(simple bool) ModelsStreamFunction {
		return func(ctx context.Context, model *Model, transcript TranscriptContext, options StreamOptions) (*AssistantMessageEventStream, error) {
			return LazyStream(ctx, model, func(ctx context.Context) (*AssistantMessageEventStream, error) {
				api, err := load(ctx)
				if err != nil {
					return nil, err
				}
				if simple {
					return api.StreamSimple(ctx, model, transcript, options)
				}
				return api.Stream(ctx, model, transcript, options)
			}), nil
		}
	}
	api := &ProviderStreams{Stream: wrap(false), StreamSimple: wrap(true)}
	if capabilities.FetchDeferred {
		api.FetchDeferred = func(ctx context.Context, model *Model, handle DeferredHandle, options DeferredFetchOptions) (*AssistantMessageEventStream, error) {
			return LazyStream(ctx, model, func(ctx context.Context) (*AssistantMessageEventStream, error) {
				loaded, err := load(ctx)
				if err != nil {
					return nil, err
				}
				if loaded.FetchDeferred == nil {
					return nil, fmt.Errorf("API does not support deferred responses")
				}
				return loaded.FetchDeferred(ctx, model, handle, options)
			}), nil
		}
	}
	if capabilities.CancelDeferred {
		api.CancelDeferred = func(ctx context.Context, model *Model, handle DeferredHandle, options DeferredCancelOptions) error {
			loaded, err := load(ctx)
			if err != nil {
				return err
			}
			if loaded.CancelDeferred == nil {
				return fmt.Errorf("API cannot cancel deferred responses")
			}
			return loaded.CancelDeferred(ctx, model, handle, options)
		}
	}
	return api
}
