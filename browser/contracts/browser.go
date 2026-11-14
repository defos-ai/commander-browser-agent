package contracts

import "context"

// Browser provides deterministic operations on the session's retained browser.
// It owns neither a reasoning loop nor an independent browser lifetime.
type Browser interface {
	Login(context.Context, LoginRequest, CredentialSource) (LoginResult, error)
	Screenshot(context.Context, ScreenshotRequest) (Image, error)
}

// BrowserFactory lends deterministic access inside the current session callback.
// Implementations preserve its scope and do not create another session owner.
type BrowserFactory func(context.Context) (Browser, error)

type LoginRequest struct {
	URL             string
	VerificationURL string
	ExpectedText    string
}

type LoginResult struct {
	Status    string `json:"status"`
	Submitted bool   `json:"submitted"`
	Reason    string `json:"reason"`
}

// Credentials belong only to deterministic login. The executor owns and clears
// the returned buffers, including on errors; they never enter agent requests.
type Credentials struct {
	Username []byte `json:"-"`
	Password []byte `json:"-"`
}

func (Credentials) String() string   { return "private browser login credentials" }
func (Credentials) GoString() string { return "private browser login credentials" }

// CredentialSource supplies values only after supported forms are inspected.
// OneTimeCode is requested at most once; each returned buffer belongs to login.
type CredentialSource struct {
	Credentials func(context.Context) (Credentials, error)
	OneTimeCode func(context.Context) ([]byte, error)
}

type ScreenshotRequest struct {
	Width  int
	Height int
}

// Image carries only the protected PNG and its source truncation flag.
type Image struct {
	PNG       []byte
	Truncated bool
}
