# Model availability task ownership

Pi 0.87.1 `packages/coding-agent/src/core/model-runtime.ts:286-299,323-343` rejects availability `Promise.all` on the first failure. Other promises continue until they settle or their signal is cancelled. Success requires every branch. The shared snapshot and visible error use the existing full/provider generation checks.

Pi's provider-refresh catch handlers add failures to the result map as they settle (`model-runtime.ts:722-731`). The outer refresh still waits for all selected providers because those handlers catch their individual failures.

## Go lifetime

One `modelTaskOwner` belongs to the Services-owned registry. `ModelRuntime.startBackground` and all availability fan-outs use that owner. The registry's auth-check and available-model fan-outs share it, so a nested failure cannot remain hidden behind another provider.

Each fan-out has a coordinator that publishes its first rejection and then drains every child result and joins every child. The caller stops waiting on rejection or cancellation. The coordinator remains admitted until it drains. A late sibling does not republish snapshot or error state.

`Services.Close()` stops admission, cancels the Services lifetime and waits for all admitted coordinators and background jobs. `ModelRuntime.Close()` uses the same owner for standalone `CreateModelRuntime` callers. Storage supplied by the caller remains caller-owned. A Session Runtime created with supplied Services does not close them. Call Close outside model callbacks and after active Sessions finish.

The CLI closes Services before extension transports and profiles at process exit. RPC closes Services only when it creates them. RPC background catalog work uses the same owner and keeps its original caller cancellation and timeout.

## Cancellation context

A live parent signal has one shared cancellation binding, not one retained child per operation. Request values are supplied separately for each call. Cancellation causes and deadlines remain visible. A completed operation does not cancel a retained `Done` observer. Parent cancellation removes its binding; Services closure cancels and clears remaining bindings.

This is a native lifetime mechanism for the existing Promise behavior. It does not create another model registry, another snapshot, or a second task owner for registration refreshes.

## Evidence

- `coding/model_availability_failfast_test.go`: full/provider fail-fast settlement, first-error identity, late-sibling suppression and provider error-map order.
- `internal/codingagent/model_availability_failfast_test.go`: nested auth/model fan-out failures.
- `internal/codingagent/model_tasks_test.go`: non-cooperative sibling draining, closed admission, shared cancellation without value leakage, cancellation causes/deadlines and retained observers.
- `coding/services_model_tasks_test.go`: caller-owned Services versus Session Runtime closure and standalone ModelRuntime closure.
- `cmd/pig/model_tasks_exit_test.go`: actual process exit drains model tasks before extension transports and profiles.
- `test/parity/scenarios/model-runtime-store-catalog/31-availability-failfast.toml`: exact production GetAvailable/GetError comparison against Pi while credential listing remains blocked.

The generation/snapshot tests remain in `coding/model_availability_*_test.go`. This contract does not close unrelated registration publication or legacy facade obligations.
