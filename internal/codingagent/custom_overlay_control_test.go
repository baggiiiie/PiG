package codingagent

import (
	"io"
	"testing"

	"github.com/MichaelKinsy/PiG/coding/extension"
	"github.com/MichaelKinsy/PiG/tui"
)

// Pi tui.ts:715-769 keeps a noncapturing overlay mounted while focus and
// visibility switch. The host input route follows the same state, rather than
// letting the remote overlay consume editor keys after unfocus.
func TestRemoteOverlayHandleControlsOwnInputRoute(t *testing.T) {
	mode := &InteractiveMode{tuiInst: tui.NewWithOutput(io.Discard, 100, 40)}
	ui := &ExtUIContext{m: mode}
	value, ok := ui.RunRemoteOverlay(extension.RemoteOverlayOptions{Overlay: true, Layout: &extension.OverlayLayout{NonCapturing: true}}, nil, func(handle extension.RemoteOverlayHandle) {
		overlay := handle.(*customOverlay)
		control := func(action string, hidden, wantHidden, wantFocused bool) {
			t.Helper()
			state, err := overlay.Control(t.Context(), action, hidden)
			if err != nil || state.Hidden != wantHidden || state.Focused != wantFocused {
				t.Fatalf("%s: state=%+v err=%v", action, state, err)
			}
			route, _ := mode.modalRoute()
			if (route != nil) != wantFocused {
				t.Fatalf("%s: input route active=%v, focused=%v", action, route != nil, wantFocused)
			}
		}
		control("", false, false, false)
		control("focus", false, false, true)
		control("unfocus", false, false, false)
		control("setHidden", true, true, false)
		control("focus", false, true, false)
		control("setHidden", false, false, false)
		control("focus", false, false, true)
		handle.Close("closed")
	})
	if !ok || value != "closed" {
		t.Fatalf("overlay result = %v, %v", value, ok)
	}
	if route, _ := mode.modalRoute(); route != nil {
		t.Fatal("overlay input route survived close")
	}
}
