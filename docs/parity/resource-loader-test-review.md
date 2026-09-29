# Resource-loader and SDK skills test review

Reference: Pi 0.87.1 in `.upstream/v0.87.1`.

- `packages/coding-agent/test/resource-loader.test.ts`: SHA-256 `1ebd3d0a1f10f0a2dec6fdbab7a75bc8c85c681190bcba1ae9e3d6b2a68b2f6d`.
- `packages/coding-agent/test/sdk-skills.test.ts`: SHA-256 `fdba99833bf6dd8346c3acdc7c77e156fab5ff451c3cc2b7a93295de60ac2307`.

## Resource-loader cases

All 42 case sites have executable coverage. The table cites each upstream case once. Most ports already exist in the release baseline. The directory-candidate port now also checks the upstream no-warning assertion. Native collectors, loaders and runners cover resource selection and application. The three constructor/override callback cases execute the shipped Node SDK through the production subprocess Host; they do not claim a native Go `DefaultResourceLoader` API.

Test aliases:

| Alias | Go evidence |
|---|---|
| SDKOptions | `cmd/pig/configured_resources_test.go#TestResourceLoaderUpstreamSDKOptions` |
| Discovery | `cmd/pig/configured_resources_test.go#TestResourceLoaderUpstreamDiscovery` |
| Precedence | `cmd/pig/configured_resources_test.go#TestResourceLoaderUpstreamProjectPrecedence` and `internal/codingagent/theme_precedence_test.go#TestResourceLoaderUpstreamThemePrecedence` |
| Symlinks | `cmd/pig/configured_resources_test.go#TestResourceLoaderUpstreamSymlinkedExtensions` |
| Trust | `cmd/pig/configured_resources_test.go#TestResourceLoaderUpstreamPreTrustExtensions` |
| Conflicts | `cmd/pig/configured_resources_test.go#TestResourceLoaderUpstreamExtensionConflicts` |
| ContextOptions | `cmd/pig/upstream_resource_context_options_test.go#TestUpstreamResourceLoaderContextOptions` |
| ContextFiles | `internal/codingagent/upstream_resource_context_test.go#TestUpstreamResourceLoaderContextFiles` |
| Untrusted | `cmd/pig/upstream_resource_context_options_test.go#TestUpstreamResourceLoaderUntrustedProject` |
| PromptSources | `cmd/pig/resource_prompts_test.go#TestResourceLoaderUpstreamSystemPromptSources` |
| ExtensionResources | `internal/codingagent/reload_resources_test.go#TestResourceLoaderUpstreamExtensionResources` |
| PackageMetadata | `cmd/pig/reload_resources_test.go#TestResourceLoaderUpstreamPackageMetadata` |

| Upstream line | Case | Evidence/subtest |
|---:|---|---|
| 34 | Empty results before reload | SDKOptions / initial results |
| 43 | Discover agentDir skills | Discovery / skill |
| 62 | Ignore extra Markdown in discovered skill directories | Discovery / skill siblings |
| 83 | Discover agentDir prompts | Discovery / prompt |
| 102 | Invalid prompt frontmatter with valid siblings | Discovery / invalid prompt |
| 123 | Project resources win name collisions | Precedence |
| 183 | Load symlinked project/user extensions once; retain project alias | Symlinks |
| 213 | Preload user extensions before trust; reuse their instance | Trust |
| 260 | Keep both extensions when commands collide; ordered invocation names | Conflicts / commands |
| 326 | Honor overrides for all auto-discovered resource kinds | Discovery / disabled |
| 370 | Discover AGENTS.md | ContextOptions / discover AGENTS.md |
| 380 | Override preference and ancestor layering | ContextFiles / override-layering |
| 399 | Ignore directory candidates without warnings | ContextFiles / directory-candidates |
| 417 | noContextFiles | ContextOptions / noContextFiles |
| 429 | Discover project SYSTEM.md | PromptSources / SYSTEM.md discovery |
| 440 | Untrusted project skips executable/configuration resources, not context | Untrusted |
| 486 | Discover APPEND_SYSTEM.md | PromptSources / APPEND_SYSTEM.md discovery |
| 499 | Project system prompt source | PromptSources / discovered project SYSTEM.md |
| 512 | Global system prompt source | PromptSources / discovered global SYSTEM.md |
| 523 | No source for literal system prompt | PromptSources / literal system prompt |
| 531 | File-backed system prompt option | PromptSources / file-backed system prompt |
| 542 | Discovered append prompt source | PromptSources / discovered APPEND_SYSTEM.md |
| 555 | No source for literal append prompt | PromptSources / literal append prompt |
| 563 | Only file-backed mixed append options have sources | PromptSources / mixed append prompts |
| 580 | Extension skill/prompt source metadata | ExtensionResources / false |
| 645 | File-URL skill resource and source metadata | ExtensionResources / true |
| 685 | Preserve package and extension metadata for skills/prompts/themes | PackageMetadata |
| 786 | noSkills suppresses discovery | Discovery / no skills |
| 805 | noSkills retains additional paths | Discovery / additional skill |
| 831 | skillsOverride | SDKOptions / skillsOverride |
| 855 | systemPromptOverride | SDKOptions / systemPromptOverride |
| 868 | Tool conflict diagnostics | Conflicts / tools |
| 912 | Explicit CLI extension wins tools; commands remain addressable | Conflicts / explicit CLI |
| 1010 | Nested worktree shadows main context | ContextFiles / nested-duplicate |
| 1020 | Nested worktree inherits missing context | ContextFiles / nested-inherit |
| 1029 | Different filenames do not shadow | ContextFiles / different-filename |
| 1042 | Bare-layout container context remains | ContextFiles / bare-container |
| 1065 | Ancestors above the main repository remain | ContextFiles / ancestors-above-main |
| 1077 | Sibling worktree context | ContextFiles / sibling-worktree |
| 1095 | Submodule retains superproject context | ContextFiles / submodule |
| 1114 | Ordinary repository keeps ancestor walk | ContextFiles / ordinary-repo |
| 1130 | Missing gitdir target keeps ancestor walk | ContextFiles / missing-gitdir-target |

No case is proposed as designed out. The `.pi` → `.pig` path spelling follows D2. Pre-trust module evaluation uses a file counter instead of process-global state because the native Host executes extensions in subprocesses. Both counters assert exactly one evaluation.

## SDK skills cases and native blocker

`cmd/pig/sdk_skills_upstream_test.go#TestUpstreamSDKSkillsSubprocess` ports all three cases through the production extension loader. Each case uses a child directory containing the upstream test skill, an in-memory SessionManager and an independent child session. Each child is disposed before the Host shuts down.

| Upstream line | Case | Subtest |
|---:|---|---|
| 41 | Default discovery exposes test-skill | default discovery |
| 53 | A supplied empty loader suppresses discovery and diagnostics | empty supplied loader |
| 79 | A supplied custom skill retains every field and sourceInfo; diagnostics are empty | custom supplied skill |

`coding/extension/host/subprocess/runtime_node_child_session_test.go#TestNodeSDKSessionDefaultsAndSkillsMatchPi` also runs the original assertions against the installed Pi SDK and shipped Node SDK. The new tests cross the Host boundary instead of importing the shim directly. They exercise both existing `isolated` and `shared-ok` configuration values, without claiming that Node factories share a process.

The file remains **partial**. Native `coding.NewSession` has no resource-loader construction option and leaves `session.promptResources` unset. `Session.SetPromptResources` is a later assignment of collections; calling it in a test would bypass the default-discovery and constructor-injection contracts. This is an applicable missing native behavior, not a proposed designed-out case. The existing approved 0.3.x known-gap policy remains in force for this file.

Native reproduction with the same `test-skill/SKILL.md` fixture:

1. Create `coding.Services` with both `CWD` and `AgentDir` set to the fixture root.
2. Create `coding.NewInMemorySessionManager(root)`.
3. Call `coding.NewSession(services, coding.SessionOptions{SessionManager: manager})`.
4. In a `package coding` test, assert that `session.promptResources.Load()` contains `test-skill`.

The test compiles and fails with `native session has no resource collection after construction`. The native constructor needs an owned, injectable resource-loading path shared with the CLI. Adding a test-only loader or discovering just this fixture directory does not close the gap. Native closure remains assigned to the release lead's `FOLLOWUP-extensions` obligation.

## Verification

The focused tests pass with Go 1.27.1 on Linux. The installed Node is v26.7.0; this is not qualification on the documented Node 24.19.0 toolchain. The paired SDK test uses the existing local Pi 0.87.1 installation.

```sh
go build ./...
go vet ./cmd/pig ./internal/codingagent
go test -count=1 ./cmd/pig ./internal/codingagent -run 'Test(ResourceLoaderUpstream|UpstreamResourceLoader|UpstreamSDKSkillsSubprocess)'
go test -count=1 ./coding/extension/host/subprocess -run '^TestNodeSDKSessionDefaultsAndSkillsMatchPi$'
make test-porting-release
```

The release gate passes with the resource-loader deferral removed and the SDK native deferral retained. `go tool golangci-lint config verify` passes. Repository lint has unrelated baseline blockers: `make lint-changed` includes `examples/extensions/plan-mode`, which the main module does not contain; `make lint` reports a goimports finding in `coding/extension/host/subprocess/owner_upgrade_test.go`. Neither blocker is changed in this lane.

Mutation evidence:

- A compiling Go overlay adds a warning when `loadContextFileFromDir` sees a directory, without changing its fallback result. `TestUpstreamResourceLoaderContextFiles/directory-candidates` fails on both candidate paths. This detects the assertion missing from the baseline.
- A Node load hook replaces `let resourceLoader = options.resourceLoader` with `let resourceLoader` in the shipped `sdk-bundle/sdk.js`. Both supplied-loader subtests fail because they receive the discovered `test-skill` instead of the exact supplied collection. The production module still loads and creates a session; these are behavioral failures, not loader/compiler failures.
- A Node load hook replaces `await resourceLoader.reload()` with `void 0` in the shipped `sdk-bundle/sdk.js`. The default-discovery subtest fails with an empty skills collection.
- Removing the mutations restores the focused green results. No production file or generated runtime archive is modified for these probes.
