package codingagent

import (
	"reflect"
	"strings"
	"testing"

	"github.com/MichaelKinsy/PiG/tui"
)

// Ports the four exact Pi 0.87.1 trust-selector cases.
func TestTrustSelectorUpstream(t *testing.T) {
	previous := tui.GetKeybindings()
	t.Cleanup(func() { tui.SetKeybindings(previous) })
	DefaultKeybindingsManager()
	render := func(component *TrustSelectorComponent) string {
		return stripANSITest(strings.Join(component.Render(120), "\n"))
	}
	contains := func(t *testing.T, output string, expected ...string) {
		t.Helper()
		for _, text := range expected {
			if !strings.Contains(output, text) {
				t.Fatalf("render %q is missing %q", output, text)
			}
		}
	}
	// packages/coding-agent/test/trust-selector.test.ts:17
	t.Run("keeps the saved trusted decision marked while browsing", func(t *testing.T) {
		selector := NewTrustSelectorComponent(TrustSelectorOptions{Cwd: "/project", SavedDecision: &ProjectTrustStoreEntry{Path: "/project", Decision: true}, ProjectTrusted: true})
		contains(t, render(selector), "Saved decision: trusted (/project)", "Current session: trusted", "→ ✓ Trust")
		selector.HandleInput("\x1b[B")
		text := render(selector)
		contains(t, text, "✓ Trust", "→   Trust parent folder (/)")
		if strings.Contains(text, "✓ Do not trust") {
			t.Fatalf("saved marker moved: %q", text)
		}
	})
	// packages/coding-agent/test/trust-selector.test.ts:38
	t.Run("selects a trust decision", func(t *testing.T) {
		var selected *TrustSelection
		selector := NewTrustSelectorComponent(TrustSelectorOptions{Cwd: "/project", OnSelect: func(value TrustSelection) { selected = &value }})
		selector.HandleInput("\n")
		want := &TrustSelection{Trusted: true, Updates: []ProjectTrustUpdate{{Path: "/project", Decision: new(true)}}}
		if !reflect.DeepEqual(selected, want) {
			t.Fatalf("selection=%+v, want %+v", selected, want)
		}
	})
	// packages/coding-agent/test/trust-selector.test.ts:53
	t.Run("labels saved ancestor decisions as inherited", func(t *testing.T) {
		selector := NewTrustSelectorComponent(TrustSelectorOptions{Cwd: "/parent/project/nested", SavedDecision: &ProjectTrustStoreEntry{Path: "/parent", Decision: true}, ProjectTrusted: true})
		contains(t, render(selector), "Saved decision: trusted (inherited from /parent)")
	})
	// packages/coding-agent/test/trust-selector.test.ts:67
	t.Run("adds a trust parent option", func(t *testing.T) {
		var selected *TrustSelection
		selector := NewTrustSelectorComponent(TrustSelectorOptions{Cwd: "/parent/project", SavedDecision: &ProjectTrustStoreEntry{Path: "/parent", Decision: true}, ProjectTrusted: true, OnSelect: func(value TrustSelection) { selected = &value }})
		contains(t, render(selector), "Saved decision: trusted (inherited from /parent)", "✓ Trust parent folder (/parent)")
		selector.HandleInput("\n")
		want := &TrustSelection{Trusted: true, Updates: []ProjectTrustUpdate{{Path: "/parent", Decision: new(true)}, {Path: "/parent/project", Decision: nil}}}
		if !reflect.DeepEqual(selected, want) {
			t.Fatalf("selection=%+v, want %+v", selected, want)
		}
	})
}
