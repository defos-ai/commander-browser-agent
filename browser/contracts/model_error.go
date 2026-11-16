package contracts

import "errors"

// Model errors contain fixed host-owned diagnostics only. Provider response
// bodies, headers, endpoint values and credentials never belong in these errors.
var (
	ErrModelRequestRejected = errors.New("model provider rejected the browser request; verify model access and Chat Completions tool support")
	ErrModelCredentials     = errors.New("model provider denied access; verify the selected provider's managed API key and model permissions")
	ErrModelRateLimited     = errors.New("model provider rate limit reached; wait for the limit to reset")
	ErrModelUnavailable     = errors.New("model provider could not be reached or is unavailable; check connectivity and the selected endpoint")
	ErrModelResponse        = errors.New("model provider response could not be accepted safely")
	ErrModelRequestPolicy   = errors.New("native model request was refused by the browser gateway")
	ErrModelRequestLimit    = errors.New("browser model request allowance was exhausted")
)
