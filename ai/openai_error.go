package ai

// Ports packages/ai/src/api/openai-completions.ts (SDK HTTP error extraction).
// Ports packages/ai/src/api/openai-responses.ts (SDK HTTP error extraction).

import (
	"encoding/json"
	"fmt"

	"github.com/MichaelKinsy/PiG/internal/jsonstringify"
)

// openAIHTTPError reconstructs openai/core/error.APIError.generate's message and inner error object. Pi's shared error-body policy owns status/body composition and truncation.
func openAIHTTPError(status int, raw []byte, prefix ...string) error {
	var envelope map[string]json.RawMessage
	parsed := json.Valid(raw) && jsonValueTruthy(raw)
	if parsed {
		canonical, err := jsonstringify.Canonicalize(raw)
		if err == nil {
			_ = json.Unmarshal(canonical, &envelope)
		}
	}
	sdkError := envelope["error"]
	message, hasMessage := openAIStreamErrorMessage(sdkError)
	if !hasMessage && !parsed {
		message = string(raw)
	}
	if message == "" {
		message = fmt.Sprintf("%d status code (no body)", status)
	} else {
		message = fmt.Sprintf("%d %s", status, message)
	}
	return &providerError{status: new(status), body: sdkError, message: message, prefix: prefix}
}
