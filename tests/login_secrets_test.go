package browser_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/defos-ai/commander-browser-agent/browser/contracts"
	"github.com/defos-ai/commander-browser-agent/internal/browsing"
	"github.com/defos-ai/commander-browser-agent/internal/commander"
	"github.com/defos-ai/commander/framework/agent/action"
)

func TestLoginCredentialSourcePreservesDeliveryAndCancellation(t *testing.T) {
	const usernameName, passwordName, codeName = "fixture-login-username", "fixture-login-password", "fixture-login-otp"
	providerFailure := errors.New("private secret provider failure")
	for _, test := range []struct {
		name          string
		transient     bool
		failAt        int
		providerError error
		cancel        bool
		wantError     error
	}{
		{name: "username_failure", failAt: 1, providerError: providerFailure, wantError: action.ErrSecretUnavailable},
		{name: "password_failure", failAt: 2, providerError: providerFailure, wantError: action.ErrSecretUnavailable},
		{name: "transient_expiry", transient: true, failAt: 1, providerError: context.DeadlineExceeded, wantError: action.ErrSecretUnavailable},
		{name: "transient_cancellation", transient: true, failAt: 1, providerError: providerFailure, cancel: true, wantError: context.Canceled},
		{name: "credentials_delivered"},
		{name: "transient_delivered", transient: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			calls := 0
			provider := &loginSecretProvider{request: func(ctx context.Context, name string, options action.SecretRequestOptions) ([]byte, error) {
				calls++
				wantName, wantType := usernameName, action.SecretReusable
				if test.transient {
					wantName, wantType = codeName, action.SecretTransient
				} else if calls == 2 {
					wantName = passwordName
				}
				if name != wantName || options.Type != wantType || options.Default != nil {
					t.Error("login changed the secret purpose, delivery type or default")
				}
				if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 3*time.Minute {
					t.Error("login secret delivery lost its bounded wait")
				}
				value := []byte("owned-login-delivery-" + name)
				if calls == test.failAt {
					if test.cancel {
						cancel()
					}
					return value, test.providerError
				}
				return value, nil
			}}
			budget := browsing.NewBudget(action.WithLogs(action.WithSecrets(ctx, provider), provider), time.Minute)
			defer budget.Close()
			source := commander.LoginCredentialSource(budget, usernameName, passwordName, codeName)
			var values contracts.Credentials
			var code []byte
			var err error
			if test.transient {
				code, err = source.OneTimeCode(budget.Context())
			} else {
				values, err = source.Credentials(budget.Context())
			}
			defer clear(values.Username)
			defer clear(values.Password)
			defer clear(code)
			if !errors.Is(err, test.wantError) {
				t.Fatal("login changed delivery failure or cancellation classification")
			}
			wantCalls := 2
			if test.transient || test.failAt == 1 {
				wantCalls = 1
			}
			if calls != wantCalls {
				t.Fatal("login requested another secret after failure or repeated a delivery")
			}
			if err != nil {
				if values.Username != nil || values.Password != nil || code != nil {
					t.Fatal("failed login delivery returned partial credentials or a code")
				}
			} else if test.transient {
				if string(code) != "owned-login-delivery-"+codeName {
					t.Fatal("login lost the owned transient delivery")
				}
			} else if string(values.Username) != "owned-login-delivery-"+usernameName || string(values.Password) != "owned-login-delivery-"+passwordName {
				t.Fatal("login lost the owned credential pair")
			}
			if test.cancel != errors.Is(budget.Context().Err(), context.Canceled) || !test.cancel && budget.Context().Err() != nil {
				t.Fatal("delivery failure canceled a live budget or lost caller cancellation")
			}
		})
	}
}

type loginSecretProvider struct {
	request func(context.Context, string, action.SecretRequestOptions) ([]byte, error)
}

func (*loginSecretProvider) Secret(string) ([]byte, error) {
	return nil, action.ErrSecretUnavailable
}

func (provider *loginSecretProvider) RequestSecretWithOptions(ctx context.Context, name string, options action.SecretRequestOptions) ([]byte, error) {
	return provider.request(ctx, name, options)
}

func (*loginSecretProvider) Log(ctx context.Context, _ string) error { return ctx.Err() }
