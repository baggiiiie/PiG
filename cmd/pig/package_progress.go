package main

import (
	"fmt"
	"io"
)

// Ports packages/coding-agent/src/core/package-manager.ts (ProgressEvent, ProgressCallback, withProgress).

// ProgressEvent describes one package operation. Completion omits Message.
type ProgressEvent struct {
	Type    string
	Action  string
	Source  string
	Message *string
}

// ProgressCallback runs synchronously before an operation starts and before its result returns.
type ProgressCallback func(ProgressEvent)

func emitProgress(callback ProgressCallback, event ProgressEvent) {
	if callback != nil {
		callback(event)
	}
}

func finishPackageProgress(callback ProgressCallback, action, source string, err error) error {
	event := ProgressEvent{Type: "complete", Action: action, Source: source}
	if err != nil {
		event.Type = "error"
		event.Message = new(err.Error())
	}
	emitProgress(callback, event)
	return err
}

func withProgress(callback ProgressCallback, action, source, message string, operation func() error) error {
	emitProgress(callback, ProgressEvent{Type: "start", Action: action, Source: source, Message: &message})
	return finishPackageProgress(callback, action, source, operation())
}

// Package CLI output prints starts; the command caller owns success and failure diagnostics.
func packageProgressPrinter(writer io.Writer) ProgressCallback {
	return func(event ProgressEvent) {
		if event.Type == "start" && event.Message != nil {
			_, _ = fmt.Fprintln(writer, *event.Message)
		}
	}
}
