package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestOpenAICompat_OpenDesignQuota(t *testing.T) {
	old := util.OpenDesignAuthBaseURL
	util.OpenDesignAuthBaseURL = "https://auth.test"
	t.Cleanup(func() { util.OpenDesignAuthBaseURL = old })
	for _, tc := range []struct {
		name              string
		status            int
		session, balance  string
		scoped            bool
		wantStatus, calls int
	}{
		{"402 exhausted", 402, "secret", "0", true, 429, 2},
		{"402 wallet funded", 402, "secret", "1", false, 402, 2},
		{"402 no session", 402, "", "0", false, 402, 0},
		{"429 exhausted", 429, "secret", "0", true, 429, 2},
		{"503 transient", 503, "secret", "0", false, 503, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			auth := newCommandCodeTestAuth(map[string]string{"base_url": "https://amr-link.open-design.ai/provider/v1", "opendesign_session": tc.session, "opendesign_workspace_id": "ws"})
			// Reuse the executor fixture while replacing its transport for dashboard requests.
			err := runOpenDesign(t, auth, func(req *http.Request) (*http.Response, error) {
				body := `{"error":{"code":"insufficient_balance"}}`
				status := tc.status
				if req.URL.Host == "auth.test" {
					calls++
					if req.Header.Get("Cookie") != "__Secure-better-auth.session_token=secret" || req.Header.Get("x-vela-workspace-id") != "ws" {
						t.Errorf("dashboard headers: %v", req.Header)
					}
					status = http.StatusOK
					if strings.HasSuffix(req.URL.Path, "coding-plan-usage") {
						body = fmt.Sprintf(`{"windows":[{"durationSeconds":18000,"remainingCredits":"0","usedCredits":"50000","limitCredits":"50000","resetsAt":%q}]}`, time.Now().Add(2*time.Hour).UTC().Format(time.RFC3339Nano))
					} else {
						body = fmt.Sprintf(`{"balanceUsd":%q}`, tc.balance)
					}
				} else if tc.status == 503 {
					body = `{"error":{"code":"workspace_billing_scope_unavailable"}}`
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
			})
			var se statusErr
			if !errors.As(err, &se) {
				t.Fatalf("expected statusErr: %v", err)
			}
			if se.StatusCode() != tc.wantStatus || se.IsCredentialScoped() != tc.scoped || calls != tc.calls {
				t.Fatalf("status=%d scoped=%v calls=%d", se.StatusCode(), se.IsCredentialScoped(), calls)
			}
			if tc.scoped {
				if se.RetryAfter() == nil || *se.RetryAfter() < 119*time.Minute || *se.RetryAfter() > 2*time.Hour {
					t.Fatalf("retryAfter=%v", se.RetryAfter())
				}
			} else if se.RetryAfter() != nil {
				t.Fatalf("retryAfter=%v", se.RetryAfter())
			}
		})
	}
}

func runOpenDesign(t *testing.T, auth *coreauth.Auth, transport func(*http.Request) (*http.Response, error)) error {
	t.Helper()
	e := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", roundTripperFunc(transport))
	req, opts := session.Enrich(coreexec.Request{Model: "test", Payload: []byte(`{"model":"test","messages":[{"role":"user","content":"hi"}]}`)}, coreexec.Options{SourceFormat: translator.FromString("openai")})
	_, err := e.Execute(ctx, auth, req, opts)
	return err
}
