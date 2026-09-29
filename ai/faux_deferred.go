package ai

// Ports packages/ai/src/providers/faux.ts

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"time"
)

type FauxDeferredConfig struct {
	PendingFetches float64
	PollAfterMS    *int64
}
type fauxDeferredResponse struct {
	handle         DeferredHandle
	step           FauxResponseStep
	context        TranscriptContext
	options        StreamOptions
	model          *Model
	pendingFetches float64
	cancelled      bool
	final          *FauxResponse
}

// Provider exposes the model-aware provider definition for an explicit Models collection.
func (p *fauxProvider) Provider() *ModelsProvider { return p.definition }
func (p *fauxProvider) DeferredFetchCount() int   { return int(p.state.DeferredFetchCount.Load()) }
func (p *fauxProvider) CancelledDeferred() []DeferredHandle {
	p.mu.Lock()
	defer p.mu.Unlock()
	result := make([]DeferredHandle, len(p.cancelledDeferred))
	for i, handle := range p.cancelledDeferred {
		result[i] = *cloneDeferredHandle(&handle)
	}
	return result
}
func (p *fauxProvider) makeDefinition() *ModelsProvider {
	stream := func(ctx context.Context, model *Model, transcript TranscriptContext, options StreamOptions) (*AssistantMessageEventStream, error) {
		return p.streamModel(ctx, model, transcript, options)
	}
	return CreateProvider(CreateProviderOptions{ID: p.ID(), Auth: ProviderAuth{APIKey: &APIKeyAuth{Name: "Faux", Resolve: func(context.Context, APIKeyAuthInput) (*AuthResult, error) { return &AuthResult{}, nil }}}, Models: p.models, API: &ProviderStreams{Stream: stream, StreamSimple: stream, FetchDeferred: p.fetchDeferred, CancelDeferred: p.cancelDeferred}})
}

func (p *fauxProvider) submitDeferred(model *Model, request TranscriptContext, options StreamOptions, step FauxResponseStep) DeferredHandle {
	handle := DeferredHandle{Provider: modelProviderID(model), ModelID: model.ID, API: model.ProviderMeta.API, ID: fmt.Sprintf("deferred:%d:%d", time.Now().UnixNano(), rand.Uint64())}
	pending := 0.0
	if p.cfg.Deferred != nil {
		pending = math.Max(0, math.Floor(p.cfg.Deferred.PendingFetches))
		if p.cfg.Deferred.PollAfterMS != nil {
			handle.PollAfterMS = new(*p.cfg.Deferred.PollAfterMS)
		}
	}
	p.mu.Lock()
	p.deferredResponses[handle.ID] = &fauxDeferredResponse{handle: handle, step: step, context: request, options: options, model: model, pendingFetches: pending}
	p.mu.Unlock()
	return handle
}

func (p *fauxProvider) fetchDeferred(ctx context.Context, model *Model, handle DeferredHandle, options DeferredFetchOptions) (*AssistantMessageEventStream, error) {
	p.state.DeferredFetchCount.Add(1)
	options.Deferred = nil
	return LazyStream(ctx, model, func(ctx context.Context) (*AssistantMessageEventStream, error) {
		if options.OnResponse != nil {
			if err := options.OnResponse(ctx, ProviderResponse{Status: 200, Headers: map[string]string{}}, model); err != nil {
				return nil, err
			}
			options.OnResponse = nil
		}
		p.mu.Lock()
		entry := p.deferredResponses[handle.ID]
		if entry == nil || entry.handle.Provider != handle.Provider || entry.handle.ModelID != handle.ModelID || entry.handle.API != handle.API {
			p.mu.Unlock()
			return nil, fmt.Errorf("Unknown faux deferred response: %s", handle.ID)
		}
		if entry.cancelled {
			p.mu.Unlock()
			return nil, fmt.Errorf("Faux deferred response was cancelled: %s", handle.ID)
		}
		if entry.pendingFetches > 0 {
			entry.pendingFetches--
			pending := FauxResponse{Content: []FauxContentBlock{}, StopReason: "deferred", Deferred: cloneDeferredHandle(&entry.handle), usage: &Usage{}}
			p.mu.Unlock()
			step := FauxStaticStep(pending)
			return p.streamStep(ctx, model, entry.context, options.StreamOptions, &step)
		}
		final := entry.final
		submissionOptions := entry.options
		submissionOptions.Deferred = nil
		submissionOptions.OnResponse = nil
		submissionOptions.Signal = nil
		p.mu.Unlock()
		if final == nil {
			var response FauxResponse
			var err error
			switch {
			case entry.step.Static != nil:
				response = *entry.step.Static
			case entry.step.Factory != nil:
				response, err = entry.step.Factory(entry.context, submissionOptions, &p.state, entry.model)
			default:
				err = fmt.Errorf("faux response step has neither static nor factory")
			}
			if err != nil {
				response = FauxResponse{Content: []FauxContentBlock{}, StopReason: "error", ErrorMessage: err.Error(), usage: &Usage{}}
			} else {
				response.usage = p.estimateUsage(entry.context, submissionOptions, response.Content)
			}
			p.mu.Lock()
			entry.final = &response
			final = entry.final
			p.mu.Unlock()
		}
		step := FauxStaticStep(*final)
		return p.streamStep(ctx, entry.model, entry.context, submissionOptions, &step)
	}), nil
}

func (p *fauxProvider) cancelDeferred(ctx context.Context, model *Model, handle DeferredHandle, options DeferredCancelOptions) error {
	p.mu.Lock()
	p.cancelledDeferred = append(p.cancelledDeferred, *cloneDeferredHandle(&handle))
	if entry := p.deferredResponses[handle.ID]; entry != nil {
		entry.cancelled = true
	}
	p.mu.Unlock()
	if options.OnResponse != nil {
		return options.OnResponse(ctx, ProviderResponse{Status: 200, Headers: map[string]string{}}, model)
	}
	return nil
}
