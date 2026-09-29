package subprocess

import (
	"encoding/json"
	"errors"
	"slices"

	"github.com/MichaelKinsy/PiG/ai"
)

func (h *Host) collectProviderObjectLocked(provider *nativeProviderProxy) []*nativeProviderProxy {
	if provider.registered || provider.calls != 0 || len(provider.references) != 0 || h.nativeProviderHandles[provider.declaration.Handle] != provider {
		return nil
	}
	delete(h.nativeProviderHandles, provider.declaration.Handle)
	return []*nativeProviderProxy{provider}
}
func (h *Host) releaseProviderCallbacks(providers []*nativeProviderProxy) {
	for _, provider := range providers {
		args, _ := json.Marshal(map[string]string{"key": provider.declaration.Key})
		// Closed owners release their callback table with the connection.
		_ = provider.conn.Send(&Envelope{Type: MsgNotify, Notify: &NotifyPayload{Method: "provider_release", Args: args}})
	}
}
func (h *Host) retireNativeProviderLocked(id string) []*nativeProviderProxy {
	var released []*nativeProviderProxy
	for _, providers := range h.nativeProviders {
		provider := providers[id]
		if provider == nil {
			continue
		}
		delete(providers, id)
		provider.registered = false
		provider.owner.providerNames = slices.DeleteFunc(provider.owner.providerNames, func(name string) bool { return name == id })
		provider.owner.oauthProviderNames = slices.DeleteFunc(provider.owner.oauthProviderNames, func(name string) bool { return name == id })
		if provider.declaration.OAuth != nil {
			ai.UnregisterOAuthProvider(id)
		}
		released = append(released, h.collectProviderObjectLocked(provider)...)
	}
	return released
}
func (h *Host) handleProviderReference(me *managedExt, call *CallPayload) (*CallResultPayload, error) {
	var args struct {
		Handle string `json:"handle"`
		Token  string `json:"token"`
	}
	if err := json.Unmarshal(call.Args, &args); err != nil {
		return nil, err
	}
	if args.Token == "" {
		return nil, errors.New("Provider reference requires a token")
	}
	h.mu.Lock()
	provider := h.nativeProviderHandles[args.Handle]
	if provider == nil {
		h.mu.Unlock()
		if call.Method == "provider.release" {
			return &CallResultPayload{}, nil
		}
		return nil, errors.New("Provider object owner is no longer connected")
	}
	if call.Method == "provider.retain" {
		if provider.references[me.conn] == nil {
			provider.references[me.conn] = map[string]struct{}{}
		}
		provider.references[me.conn][args.Token] = struct{}{}
	} else {
		delete(provider.references[me.conn], args.Token)
		if len(provider.references[me.conn]) == 0 {
			delete(provider.references, me.conn)
		}
	}
	released := h.collectProviderObjectLocked(provider)
	h.mu.Unlock()
	h.releaseProviderCallbacks(released)
	return &CallResultPayload{}, nil
}
