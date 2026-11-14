package commander

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/defos-ai/commander-browser-agent/browser/contracts"
	"github.com/defos-ai/commander-browser-agent/internal/browsing"
	"github.com/defos-ai/commander/framework/agent/action"
)

// ResolveLoginProfile asks only for a missing non-secret credential alias.
func ResolveLoginProfile(ctx context.Context, profile string) (string, error) {
	if profile == "" {
		question, err := action.Question(ctx, "browser_login_profile", "Which non-secret credential profile should this login use? Enter a lowercase alias such as work or personal, not a username, password or verification code.")
		if err != nil {
			return "", err
		}
		profile = strings.TrimSpace(question.Answer)
	}
	if !ValidLoginProfile(profile) {
		return "", errors.New("login credential_profile must be a non-secret lowercase alias of at most 32 characters")
	}
	return profile, nil
}

// LoginCredentialSource requests managed values only when deterministic login
// invokes its callbacks. Login owns and clears the returned credential buffers.
func LoginCredentialSource(budget *browsing.Budget, usernameName, passwordName, codeName string) contracts.CredentialSource {
	request := func(ctx context.Context, name string, secretType action.SecretType) ([]byte, error) {
		budget.Pause()
		defer budget.Resume()
		waiting, stop := context.WithTimeout(ctx, 3*time.Minute)
		defer stop()
		if secretType == action.SecretTransient {
			if err := action.Log(waiting, "Waiting for a one-time login code"); err != nil {
				return nil, err
			}
			value, err := action.RequestTransientSecret(waiting, name)
			// Request TTL/withdrawal can end before the action's wait. Keep a
			// live session available for explicit continuation in that case.
			if err != nil {
				clear(value)
				if waiting.Err() == nil {
					return nil, action.ErrSecretUnavailable
				}
				return nil, err
			}
			return value, err
		}
		return action.RequestReusableSecret(waiting, name)
	}
	return contracts.CredentialSource{
		OneTimeCode: func(ctx context.Context) ([]byte, error) {
			return request(ctx, codeName, action.SecretTransient)
		},
		Credentials: func(ctx context.Context) (contracts.Credentials, error) {
			username, err := request(ctx, usernameName, action.SecretReusable)
			if err != nil {
				clear(username)
				return contracts.Credentials{}, err
			}
			password, err := request(ctx, passwordName, action.SecretReusable)
			if err != nil {
				clear(username)
				clear(password)
				return contracts.Credentials{}, err
			}
			return contracts.Credentials{Username: username, Password: password}, nil
		},
	}
}

func ValidLoginProfile(profile string) bool {
	if len(profile) == 0 || len(profile) > 32 || profile[0] < 'a' || profile[0] > 'z' {
		return false
	}
	for _, ch := range profile {
		if ch != '-' && ch != '_' && (ch < 'a' || ch > 'z') && (ch < '0' || ch > '9') {
			return false
		}
	}
	return true
}

// LoginSecretNames addresses reusable credentials, never a verification code.
// Names are shared in Commander; this binding is not a secret-store access policy.
func LoginSecretNames(address, profile string) (username, password string, err error) {
	origin, err := browsing.LoginOrigin(address)
	if err != nil {
		return "", "", err
	}
	if !ValidLoginProfile(profile) {
		return "", "", errors.New("login credential_profile must be a non-secret lowercase alias of at most 32 characters")
	}
	digest := sha256.Sum256([]byte(origin + "\x00" + profile))
	base := "browser-login-" + profile + "-" + hex.EncodeToString(digest[:])
	return base + "-username", base + "-password", nil
}

// LoginCodeSecretName labels a transient request, never a stored code.
// Commander binds each request to its own execution and delivery identity.
func LoginCodeSecretName(address, profile string) (string, error) {
	username, _, err := LoginSecretNames(address, profile)
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(username, "-username") + "-otp", nil
}
