package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// TestExecute_CreditsExhaustionRotatesOnlyAs429 pins the contract the OpenAI-compatible
// executor relies on for Command Code's insufficient-credits rejection: a credential-scoped
// 429 with RetryAfter skips the broke credential and cools it down, while the same body
// surfaced as a 400 invalid_request_error is a request fault that stops rotation.
func TestExecute_CreditsExhaustionRotatesOnlyAs429(t *testing.T) {
	withQuotaCooldownEnabled(t)
	const provider = "credits-exhaustion-test"
	const upstream = `{"error":{"message":"You have insufficient credits to make this request.","type":"invalid_request_error","code":"BAD_REQUEST"}}`
	const rewritten = `{"error":{"message":"You have insufficient credits to make this request.","type":"insufficient_quota","code":"insufficient_quota"}}`

	for _, test := range []struct {
		name       string
		err        streamQuotaError
		wantRotate bool
	}{
		{name: "credential-scoped 429 rotates", err: streamQuotaError{customStatusError: customStatusError{code: http.StatusTooManyRequests, msg: rewritten}, credentialScoped: true}, wantRotate: true},
		{name: "400 request fault stops", err: streamQuotaError{customStatusError: customStatusError{code: http.StatusBadRequest, msg: upstream}, credentialScoped: true}, wantRotate: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager := NewManager(nil, quotaAttemptIsolationSelector{}, nil)
			model := "credits-exhaustion-model"
			brokeID := "credits-exhaustion-a-" + test.name
			healthyID := "credits-exhaustion-b-" + test.name
			for _, id := range []string{brokeID, healthyID} {
				if _, errRegister := manager.Register(context.Background(), &Auth{ID: id, Provider: provider, Status: StatusActive}); errRegister != nil {
					t.Fatalf("Register(%s) error = %v", id, errRegister)
				}
				registry.GetGlobalRegistry().RegisterClient(id, provider, []*registry.ModelInfo{{ID: model}})
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
			}
			retryAfter := 2 * time.Hour
			test.err.retryAfter = &retryAfter
			var attempts []string
			manager.RegisterExecutor(&mockCustomErrorExecutor{
				identifier: provider,
				executeFn: func(_ context.Context, selected *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
					attempts = append(attempts, selected.ID)
					if selected.ID == brokeID {
						return cliproxyexecutor.Response{}, test.err
					}
					return cliproxyexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
				},
			})

			before := time.Now()
			_, errExecute := manager.Execute(context.Background(), []string{provider}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
			if !test.wantRotate {
				if errExecute == nil || len(attempts) != 1 {
					t.Fatalf("400 must stop at the first credential: err=%v attempts=%v", errExecute, attempts)
				}
				return
			}
			if errExecute != nil || len(attempts) != 2 || attempts[1] != healthyID {
				t.Fatalf("expected failover to healthy credential: err=%v attempts=%v", errExecute, attempts)
			}
			broke, _ := manager.GetByID(brokeID)
			if broke.Quota.Reason != "credential_quota" || broke.NextRetryAfter.Before(before.Add(retryAfter)) {
				t.Fatalf("broke credential not cooled until RetryAfter: next=%v quota=%+v", broke.NextRetryAfter, broke.Quota)
			}

		})
	}
}

// TestExecute_CreditsExhaustionSurfaces429ForModelFallback checks that once every credential
// is out of credits, callers see HTTP 429 both on the attempt itself and while the pool is
// cooling down, which is the signal model-fallback reroutes on.
func TestExecute_CreditsExhaustionSurfaces429ForModelFallback(t *testing.T) {
	withQuotaCooldownEnabled(t)
	const provider = "credits-exhaustion-only-test"
	model := "credits-exhaustion-only-model"
	authID := "credits-exhaustion-only"
	manager := NewManager(nil, quotaAttemptIsolationSelector{}, nil)
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: authID, Provider: provider, Status: StatusActive}); errRegister != nil {
		t.Fatalf("Register error = %v", errRegister)
	}
	registry.GetGlobalRegistry().RegisterClient(authID, provider, []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(authID) })
	retryAfter := time.Hour
	attempts := 0
	manager.RegisterExecutor(&mockCustomErrorExecutor{
		identifier: provider,
		executeFn: func(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
			attempts++
			return cliproxyexecutor.Response{}, streamQuotaError{
				customStatusError: customStatusError{code: http.StatusTooManyRequests, msg: `{"error":{"message":"You have insufficient credits to make this request.","type":"insufficient_quota","code":"insufficient_quota"}}`, retryAfter: &retryAfter},
				credentialScoped:  true,
			}
		},
	})

	for i := 0; i < 2; i++ {
		_, errExecute := manager.Execute(context.Background(), []string{provider}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
		if got := statusCodeFromError(errExecute); got != http.StatusTooManyRequests {
			t.Fatalf("call %d status = %d (%v), want 429", i, got, errExecute)
		}
	}
	if attempts != 1 {
		t.Fatalf("cooling credential was retried: attempts = %d", attempts)
	}
}
