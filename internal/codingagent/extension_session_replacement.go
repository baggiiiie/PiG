package codingagent

import (
	"context"
	"errors"

	"github.com/MichaelKinsy/PiG/coding/extension"
)

// extensionSessionController supplies the Session-owned replacement operations
// without making the terminal own persistence or importing coding.Session.
type extensionSessionController interface {
	ExtensionCommandActions() extension.CommandActions
}

// sessionImporter supplies the Session-owned /import replacement.
type sessionImporter interface {
	ImportFromJsonl(ctx context.Context, inputPath, cwdOverride string) (extension.CancelledResult, error)
}

type sessionRebinder interface {
	SetBeforeSessionReplacement(func(context.Context) error)
	SetRebindSession(func(context.Context, extension.SessionStartEvent) error)
}

// replaceSessionFromCommand awaits the same Session owner used by extension commands while servicing extension dialogs and event barriers on the input loop.
func (m *InteractiveMode) replaceSessionFromCommand(ctx context.Context, operation, target string) error {
	handle, ok := m.opts.SessionHandle.(extensionSessionController)
	if !ok {
		return errors.New("session replacement is unavailable")
	}
	m.bindSessionRebind()
	actions := handle.ExtensionCommandActions()
	var result extension.CancelledResult
	text := ""
	if operation == "fork" {
		if entry, ok := m.currentSession().EntryByID(target); ok {
			if message, ok := entry.AsMessage(); ok {
				text = extractMessageText(message)
			}
		}
	}
	err := m.awaitExtensionUI(ctx, func(ctx context.Context) error {
		var err error
		switch operation {
		case "new":
			result, err = actions.NewSessionContext(ctx, nil)
		case "resume":
			result, err = actions.SwitchSessionContext(ctx, target, nil)
		case "fork", "clone":
			position := "before"
			if operation == "clone" {
				position = "at"
			}
			result, err = actions.ForkContext(ctx, target, &extension.ForkOptions{Position: position})
		}
		return err
	})
	if err != nil {
		return err
	}
	if result.Cancelled {
		return errSessionReplacementCancelled
	}
	if operation == "fork" {
		m.editor.SetText(text)
	}
	return nil
}

func (m *InteractiveMode) bindSessionRebind() {
	if handle, ok := m.opts.SessionHandle.(sessionRebinder); ok {
		handle.SetBeforeSessionReplacement(func(ctx context.Context) error {
			return m.runOnMainAndWait(ctx, m.settleActiveRun)
		})
		handle.SetRebindSession(func(ctx context.Context, event extension.SessionStartEvent) error {
			if err := m.runOnMainAndWait(ctx, func() error {
				m.opts.SessionStartEvent = &event
				if event.Reason == "new" {
					m.restoreBuiltInHeader()
				}
				return nil
			}); err != nil {
				return err
			}
			return m.rebindCurrentSession(ctx, true)
		})
	}
}

func (m *InteractiveMode) extensionReplacementActions() extension.CommandActions {
	handle, ok := m.opts.SessionHandle.(extensionSessionController)
	if !ok {
		return extension.CommandActions{}
	}
	m.bindSessionRebind()
	actions := handle.ExtensionCommandActions()
	newSession := func(ctx context.Context, opts *extension.NewSessionOptions) (extension.CancelledResult, error) {
		if err := m.prepareExtensionSessionUI(ctx); err != nil {
			return extension.CancelledResult{}, err
		}
		result, err := actions.NewSessionContext(ctx, opts)
		return m.finishExtensionSessionUI(ctx, result, err, "new", "")
	}
	fork := func(ctx context.Context, id string, opts *extension.ForkOptions) (extension.CancelledResult, error) {
		text := ""
		if opts == nil || opts.Position != "at" {
			if entry, ok := m.currentSession().EntryByID(id); ok {
				if message, ok := entry.AsMessage(); ok {
					text = extractMessageText(message)
				}
			}
		}
		result, err := actions.ForkContext(ctx, id, opts)
		return m.finishExtensionSessionUI(ctx, result, err, "fork", text)
	}
	switchSession := func(ctx context.Context, path string, opts *extension.SwitchSessionOptions) (extension.CancelledResult, error) {
		if err := m.prepareExtensionSessionUI(ctx); err != nil {
			return extension.CancelledResult{}, err
		}
		result, err := actions.SwitchSessionContext(ctx, path, opts)
		return m.finishExtensionSessionUI(ctx, result, err, "resume", "")
	}
	return extension.CommandActions{
		NewSession: func(opts *extension.NewSessionOptions) (extension.CancelledResult, error) {
			return newSession(context.Background(), opts)
		},
		NewSessionContext: newSession,
		Fork: func(id string, opts *extension.ForkOptions) (extension.CancelledResult, error) {
			return fork(context.Background(), id, opts)
		},
		ForkContext: fork,
		SwitchSession: func(path string, opts *extension.SwitchSessionOptions) (extension.CancelledResult, error) {
			return switchSession(context.Background(), path, opts)
		},
		SwitchSessionContext: switchSession,
	}
}

func (m *InteractiveMode) prepareExtensionSessionUI(ctx context.Context) error {
	if m.tuiInst == nil {
		return nil
	}
	return m.extensionSessionUIOnMain(ctx, func() error {
		m.clearStatusIndicator("")
		return nil
	})
}

func (m *InteractiveMode) finishExtensionSessionUI(ctx context.Context, result extension.CancelledResult, err error, reason, text string) (extension.CancelledResult, error) {
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	if result.Cancelled || m.tuiInst == nil {
		return result, err
	}
	return result, m.extensionSessionUIOnMain(ctx, func() error {
		if err != nil {
			prefix := "Failed to create session"
			switch reason {
			case "fork":
				prefix = "Failed to fork session"
			case "resume":
				prefix = "Failed to resume session"
			}
			return m.handleFatalRuntimeError(prefix, err)
		}
		switch reason {
		case "fork":
			m.editor.SetText(text)
			m.showStatus("Forked to new session")
		case "resume":
			m.showStatus("Resumed session")
		}
		m.tuiInst.Render()
		return nil
	})
}

func (m *InteractiveMode) extensionSessionUIOnMain(ctx context.Context, action func() error) error {
	done := make(chan error, 1)
	if err := m.postToMain(ctx, func() {
		if err := ctx.Err(); err != nil {
			done <- err
			return
		}
		done <- action()
	}); err != nil {
		return err
	}
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
