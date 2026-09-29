package codingagent

// Ports packages/coding-agent/src/modes/interactive/theme/theme-controller.ts
// Ports packages/tui/src/tui.ts

import (
	"context"
	"fmt"
	"io"
	"os"
	"slices"
	"sync/atomic"
	"time"

	"github.com/MichaelKinsy/PiG/tui"
)

// interactiveThemeState keeps query completion and modal previews on the input loop.
// Explicit extension selections publish their setting, active name, and opt-out atomically.
// Terminal input and lifecycle live in the driver rather than the paint-only renderer.
type interactiveThemeState struct {
	currentThemeSetting atomic.Pointer[string]
	terminalTheme       tui.TerminalTheme
	activeThemeName     atomic.Pointer[string]
	autoSyncEnabled     atomic.Bool
	output              io.Writer
	queries             []*interactiveThemeQuery
}

type interactiveThemeQuery struct {
	detection *startupThemeDetection
	done      chan struct{}
}

// getThemeSelection preserves an initial or explicit selection; otherwise it reads the current manager, including overrides and manager replacement.
// upstream: packages/coding-agent/src/modes/interactive/theme/theme-controller.ts:applyFromSettings
func (m *InteractiveMode) getThemeSelection() string {
	if setting := m.themeState.currentThemeSetting.Load(); setting != nil {
		return *setting
	}
	if manager := m.opts.SettingsManager; manager != nil {
		if setting := manager.GetThemeSetting(); setting != nil {
			return *setting
		}
		return ""
	}
	return m.opts.Settings.Theme
}

func (m *InteractiveMode) themeOutput() io.Writer {
	if m.themeState.output != nil {
		return m.themeState.output
	}
	if m.rendererOut != nil {
		return m.rendererOut
	}
	return os.Stdout
}

func (m *InteractiveMode) setAutoSync(enabled bool) {
	if m.themeState.autoSyncEnabled.Swap(enabled) == enabled {
		return
	}
	m.writeThemeNotifications(enabled)
}

func (m *InteractiveMode) writeThemeNotifications(enabled bool) {
	sequence := "\x1b[?2031l"
	if enabled {
		sequence = "\x1b[?2031h"
	}
	_, _ = io.WriteString(m.themeOutput(), sequence)
}

func (m *InteractiveMode) applyThemeName(name string, showError bool) bool {
	found := tui.ActiveThemeRegistry().Get(name) != nil
	tui.SetThemeByName(name, true)
	activeName := tui.ActiveTheme().Name
	m.themeState.activeThemeName.Store(&activeName)
	if m.tuiInst != nil {
		m.tuiInst.Invalidate()
		m.tuiInst.RequestRender()
	}
	if !found && showError {
		m.showError(fmt.Sprintf("Failed to load theme %q: Theme not found: %s\nFell back to dark theme.", name, name))
	}
	return found
}

func (m *InteractiveMode) applyTerminalTheme(terminalTheme tui.TerminalTheme) {
	if !m.themeState.autoSyncEnabled.Load() {
		return
	}
	m.themeState.terminalTheme = terminalTheme
	light, dark, ok := tui.ParseAutoThemeSetting(m.getThemeSelection())
	if !ok {
		m.setAutoSync(false)
		return
	}
	name := dark
	if terminalTheme == "light" {
		name = light
	}
	activeName := m.themeState.activeThemeName.Load()
	if activeName == nil || name != *activeName {
		m.applyThemeName(name, false)
	}
}

func (m *InteractiveMode) previewTheme(setting string) {
	terminalTheme := m.themeState.terminalTheme
	if terminalTheme == "" {
		terminalTheme = tui.DetectTerminalBackground(tui.TerminalThemeDetectionOptions{}).Theme
	}
	name, ok := tui.ResolveThemeSetting(setting, terminalTheme)
	if !ok {
		if active := m.themeState.activeThemeName.Load(); active != nil {
			name = *active
		}
	}
	if tui.ActiveThemeRegistry().Get(name) == nil {
		return
	}
	tui.SetThemeByName(name)
	m.tuiInst.Invalidate()
	m.tuiInst.RequestRender()
}

// beginThemeDetection starts both auto queries together. Scheme reports take precedence;
// OSC 11 can settle an unset setting immediately but an auto pair waits for the scheme deadline.
func (m *InteractiveMode) beginThemeDetection(output io.Writer) *interactiveThemeQuery {
	if m.themeState.output == nil {
		m.themeState.output = output
	}
	setting := m.getThemeSelection()
	detection := newStartupThemeDetection(setting, nil, m.tuiInst)
	if detection == nil {
		m.setAutoSync(false)
		m.applyThemeName(setting, true)
		return nil
	}
	detection.backgroundOnly = setting == ""
	if detection.backgroundOnly {
		m.setAutoSync(false)
	}
	q := &interactiveThemeQuery{detection: detection, done: make(chan struct{})}
	m.themeState.queries = append(m.themeState.queries, q)
	// theme.ts starts OSC 11 even when the scheme query fails, and otherwise awaits scheme precedence.
	detection.start(func(sequence string) error { _, err := io.WriteString(output, sequence); return err })
	if detection.readBackground() {
		m.finishThemeDetection(q)
	}
	return q
}

func (m *InteractiveMode) finishThemeDetection(q *interactiveThemeQuery) {
	select {
	case <-q.done:
		return
	default:
	}
	d := q.detection
	m.themeState.terminalTheme = d.terminalTheme()
	if !d.backgroundOnly {
		m.setAutoSync(true)
	}
	success := m.applyThemeName(d.themeName(), !d.backgroundOnly)
	if success && d.backgroundOnly && (d.background != nil || tui.DetectTerminalBackground(tui.TerminalThemeDetectionOptions{}).Confidence == "high") {
		name := d.themeName()
		if sm := m.opts.SettingsManager; sm != nil {
			// upstream: packages/coding-agent/src/core/settings-manager.ts:enqueueWrite
			_ = sm.SetTheme(name)
		}
		m.opts.Settings.Theme = name
	}
	close(q.done)
	m.pruneThemeQueries()
}

func (m *InteractiveMode) pruneThemeQueries() {
	m.themeState.queries = slices.DeleteFunc(m.themeState.queries, func(q *interactiveThemeQuery) bool {
		return q.detection.settled
	})
}

// consumeTerminalThemeInput precedes extension listeners, viewport input, and focused components.
// Reports are consumed even without an outstanding query or automatic selection.
func (m *InteractiveMode) consumeTerminalThemeInput(data string) bool {
	if m.tuiInst != nil && m.tuiInst.ConsumeOsc11BackgroundResponse(data) {
		for _, q := range slices.Clone(m.themeState.queries) {
			if q.detection.readBackground() {
				m.finishThemeDetection(q)
			}
		}
		return true
	}
	if scheme := tui.ParseTerminalColorSchemeReport(data); scheme != "" {
		m.applyTerminalTheme(scheme)
		// A report settles every outstanding scheme listener. Copy the pointers because
		// finishing a query can remove it from the pending background-reply queue.
		for _, q := range slices.Clone(m.themeState.queries) {
			if _, settled := q.detection.consume(data); settled {
				m.finishThemeDetection(q)
			}
		}
		return true
	}
	for _, q := range m.themeState.queries {
		if consumed, settled := q.detection.consume(data); consumed {
			if settled {
				m.finishThemeDetection(q)
			}
			m.pruneThemeQueries()
			return true
		}
	}
	return false
}

// initializeTerminalTheme awaits initial appearance before session_start, using the same decoder as the main loop.
func (m *InteractiveMode) initializeTerminalTheme(ctx context.Context, output io.Writer) error {
	q := m.beginThemeDetection(output)
	if q == nil {
		return nil
	}
	timer := time.NewTimer(startupThemeQueryTimeout)
	defer timer.Stop()
	for {
		select {
		case <-q.done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		case err := <-m.inputErrCh:
			return err
		case <-timer.C:
			q.detection.timeout()
			m.finishThemeDetection(q)
		case input, ok := <-m.inputReadCh:
			if !ok {
				return io.EOF
			}
			// Pi installs application and submit handlers only after startup setup (interactive-mode.ts:954-1028). Early input can edit text but cannot dispatch a command before session_start.
			m.handleStartupInput(input, false)
		case fn := <-m.uiTaskCh:
			fn()
		}
	}
}

// applyThemeFromSettings leaves the input loop free while the terminal answers.
// The Run lifetime owns and joins the deadline worker; all theme mutation runs on the owner loop.
func (m *InteractiveMode) applyThemeFromSettings(ctx context.Context) {
	q := m.beginThemeDetection(m.themeOutput())
	if q == nil {
		return
	}
	if m.backgroundCtx != nil {
		ctx = m.backgroundCtx
	}
	m.backgroundTasks.Go(func() {
		timer := time.NewTimer(startupThemeQueryTimeout)
		defer timer.Stop()
		select {
		case <-q.done:
		case <-ctx.Done():
		case <-timer.C:
			m.runOnMain(ctx, func() {
				if m.tuiTornDown {
					return
				}
				q.detection.timeout()
				m.finishThemeDetection(q)
			})
		}
	})
}

func (m *InteractiveMode) disposeTheme() {
	m.setAutoSync(false)
	for _, q := range m.themeState.queries {
		select {
		case <-q.done:
		default:
			close(q.done)
		}
	}
	m.themeState.queries = nil
}
