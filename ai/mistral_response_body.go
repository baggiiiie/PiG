package ai

// Ports packages/ai/src/api/mistral-conversations.ts (readMistralEvents).

import (
	"context"
	"io"
)

// mistralResponseBody preserves the abort reason when closing a blocked read yields EOF or a transport-specific error.
type mistralResponseBody struct {
	reader    io.Reader
	closeBody func()
	request   context.Context
}

func (body *mistralResponseBody) Read(p []byte) (int, error) {
	if err := context.Cause(body.request); err != nil {
		return 0, err
	}
	n, err := body.reader.Read(p)
	if cause := context.Cause(body.request); cause != nil {
		return 0, cause
	}
	return n, err
}

func (body *mistralResponseBody) Close() error {
	body.closeBody()
	return nil
}
