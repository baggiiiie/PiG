package coding

import (
	"reflect"
	"strings"
	"testing"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/MichaelKinsy/PiG/coding/extension"
	icodingagent "github.com/MichaelKinsy/PiG/internal/codingagent"
)

// Ports the resource-loader-supplied cases of
// packages/coding-agent/test/sdk-skills.test.ts ("should have empty skills when
// resource loader returns none (--no-skills)" and "should use provided skills
// when resource loader supplies them"). The Go session receives the resource
// owner's resolved skills through SetPromptResources; the bound collection
// must equal what was supplied and decide whether a /skill: command expands.
// The default-discovery case lives in internal/codingagent
// (TestUpstreamSDKSkillsDefaultDiscovery).
func TestUpstreamSDKSkillsSuppliedByResourceOwner(t *testing.T) {
	boundSkills := func(h *recoveryHarness) []*Skill {
		return h.session.promptResources.Load().skills
	}
	promptText := func(t *testing.T, h *recoveryHarness, text string) string {
		t.Helper()
		var sent string
		h.provider.responses = []scriptedResponse{func(messages []ai.Message) *ai.AssistantMessage {
			sent = modelExtensionUserText(messages)
			return fauxReply("ok", ai.StopReasonStop, 0)(messages)
		}}
		if _, err := h.session.Prompt(t.Context(), text); err != nil {
			t.Fatal(err)
		}
		return sent
	}

	t.Run("should have empty skills when resource loader returns none (--no-skills)", func(t *testing.T) {
		h := newQueueCharacterizationHarness(t, extension.Extension{}, nil)
		h.session.SetPromptResources(nil, []*Skill{})
		if got := boundSkills(h); len(got) != 0 {
			t.Fatalf("skills = %#v, want none", got)
		}
		if sent := promptText(t, h, "/skill:test-skill explain"); strings.Contains(sent, "<skill ") {
			t.Fatalf("a skill expanded with no skills supplied: %q", sent)
		}
	})

	t.Run("should use provided skills when resource loader supplies them", func(t *testing.T) {
		custom := &Skill{
			Name:                   "custom-skill",
			Description:            "A custom skill",
			Path:                   "/fake/path/SKILL.md",
			Dir:                    "/fake/path",
			SourceInfo:             icodingagent.PiSourceInfo{Path: "/fake/path/SKILL.md", Source: "sdk", Scope: "temporary", Origin: "top-level"},
			DisableModelInvocation: false,
		}
		h := newQueueCharacterizationHarness(t, extension.Extension{}, nil)
		h.session.SetPromptResources(nil, []*Skill{custom})
		if got := boundSkills(h); !reflect.DeepEqual(got, []*Skill{custom}) {
			t.Fatalf("skills = %#v, want [%#v]", got, custom)
		}
	})
}

// Ports packages/coding-agent/test/resource-loader.test.ts:34, :831 and :855
// through the Session's native resource hand-off, since PiG has no
// DefaultResourceLoader object: a Session starts with no bound prompt
// resources ("should initialize with empty results before reload"), the skills
// the resource owner supplies replace whatever discovery would find
// ("should apply skillsOverride"), and SessionOptions.SystemPrompt is the
// explicit system prompt ("should apply systemPromptOverride").
func TestUpstreamResourceLoaderNativeOverrides(t *testing.T) {
	t.Run("should initialize with empty results before reload", func(t *testing.T) {
		h := newQueueCharacterizationHarness(t, extension.Extension{}, nil)
		if resources := h.session.promptResources.Load(); resources != nil {
			t.Fatalf("prompt resources = %#v, want none before the resource owner binds any", resources)
		}
	})
	t.Run("should apply skillsOverride", func(t *testing.T) {
		injected := &Skill{Name: "injected", Description: "Injected skill", Path: "/fake/path", Dir: "/fake", SourceInfo: icodingagent.PiSourceInfo{Path: "/fake/path", Source: "custom", Scope: "temporary", Origin: "top-level"}}
		h := newQueueCharacterizationHarness(t, extension.Extension{}, nil)
		h.session.SetPromptResources(nil, []*Skill{{Name: "discovered", Description: "Discovered skill"}})
		h.session.SetPromptResources(nil, []*Skill{injected})
		skills := h.session.promptResources.Load().skills
		if len(skills) != 1 || skills[0].Name != "injected" {
			t.Fatalf("skills = %#v, want only the injected skill", skills)
		}
	})
	t.Run("should apply systemPromptOverride", func(t *testing.T) {
		s, err := NewSession(newTestServices(t), SessionOptions{SystemPrompt: "Custom system prompt"})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := s.Close(); err != nil {
				t.Error(err)
			}
		})
		if got := s.SystemPrompt(); got != "Custom system prompt" {
			t.Fatalf("system prompt = %q", got)
		}
	})
}
