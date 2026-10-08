package handlers

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

// fallbackTestExecutor fails with status (0 = succeed) and records the requested models.
type fallbackTestExecutor struct {
	id string
	// status is returned from Execute/ExecuteStream; streamStatus is sent as the first stream chunk.
	status       int
	streamStatus int

	mu     sync.Mutex
	models []string
}

func (e *fallbackTestExecutor) Identifier() string { return e.id }

func (e *fallbackTestExecutor) record(model string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.models = append(e.models, model)
}

func (e *fallbackTestExecutor) Models() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.models...)
}

func (e *fallbackTestExecutor) err(status int) error {
	return &coreauth.Error{Code: "upstream", Message: http.StatusText(status), HTTPStatus: status}
}

func (e *fallbackTestExecutor) Execute(_ context.Context, _ *coreauth.Auth, req coreexecutor.Request, _ coreexecutor.Options) (coreexecutor.Response, error) {
	e.record(req.Model)
	if e.status != 0 {
		return coreexecutor.Response{}, e.err(e.status)
	}
	return coreexecutor.Response{Payload: []byte(e.id)}, nil
}

func (e *fallbackTestExecutor) ExecuteStream(_ context.Context, _ *coreauth.Auth, req coreexecutor.Request, _ coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	e.record(req.Model)
	if e.status != 0 {
		return nil, e.err(e.status)
	}
	ch := make(chan coreexecutor.StreamChunk, 1)
	if e.streamStatus != 0 {
		ch <- coreexecutor.StreamChunk{Err: e.err(e.streamStatus)}
	} else {
		ch <- coreexecutor.StreamChunk{Payload: []byte(e.id)}
	}
	close(ch)
	return &coreexecutor.StreamResult{Chunks: ch}, nil
}

func (e *fallbackTestExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *fallbackTestExecutor) CountTokens(_ context.Context, _ *coreauth.Auth, req coreexecutor.Request, _ coreexecutor.Options) (coreexecutor.Response, error) {
	return e.Execute(context.Background(), nil, req, coreexecutor.Options{})
}

func (e *fallbackTestExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, e.err(http.StatusNotImplemented)
}

type fallbackTestSetup struct {
	handler *BaseAPIHandler
	main    *fallbackTestExecutor
	pooled  *fallbackTestExecutor
	other   *fallbackTestExecutor
}

// newFallbackTestSetup serves the main model from "fbtest-main" and the fallback model from
// "fbtest-pooled" and "fbtest-other"; the rule restricts the fallback to "fbtest-pooled".
func newFallbackTestSetup(t *testing.T, mainStatus, mainStreamStatus int, enabled bool) fallbackTestSetup {
	t.Helper()
	s := fallbackTestSetup{
		main:   &fallbackTestExecutor{id: "fbtest-main", status: mainStatus, streamStatus: mainStreamStatus},
		pooled: &fallbackTestExecutor{id: "fbtest-pooled"},
		other:  &fallbackTestExecutor{id: "fbtest-other"},
	}
	manager := coreauth.NewManager(nil, nil, nil)
	reg := registry.GetGlobalRegistry()
	for _, entry := range []struct {
		exec  *fallbackTestExecutor
		model string
	}{{s.main, "fbtest-main-model"}, {s.pooled, "fbtest-fallback-model"}, {s.other, "fbtest-fallback-model"}} {
		manager.RegisterExecutor(entry.exec)
		auth := &coreauth.Auth{ID: entry.exec.id + "-auth", Provider: entry.exec.id, Status: coreauth.StatusActive}
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("Register(%s): %v", auth.ID, err)
		}
		reg.RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: entry.model}})
		authID := auth.ID
		t.Cleanup(func() { reg.UnregisterClient(authID) })
	}
	s.handler = NewBaseAPIHandlers(&sdkconfig.SDKConfig{ModelFallback: sdkconfig.ModelFallbackConfig{
		Enabled: enabled,
		Rules: []sdkconfig.ModelFallbackRule{{
			Model:                   "FBTEST-MAIN-MODEL",
			FallbackModel:           "fbtest-fallback-model",
			FallbackProviders:       []string{" fbtest-pooled "},
			FallbackReasoningEffort: "High",
		}},
	}}, manager)
	return s
}

func TestModelFallbackReroutesWhenMainIsOutOfLimits(t *testing.T) {
	s := newFallbackTestSetup(t, http.StatusTooManyRequests, 0, true)

	body, _, errMsg := s.handler.ExecuteWithAuthManager(context.Background(), "openai", "fbtest-main-model(low)", []byte(`{"model":"fbtest-main-model"}`), "")
	if errMsg != nil {
		t.Fatalf("unexpected error: %v", errMsg.Error)
	}
	if string(body) != "fbtest-pooled" {
		t.Fatalf("body = %q, want fallback provider response", body)
	}
	if got := s.pooled.Models(); len(got) != 1 || got[0] != "fbtest-fallback-model(high)" {
		t.Fatalf("fallback models = %v, want fallback model with configured effort", got)
	}
	if got := s.other.Models(); len(got) != 0 {
		t.Fatalf("provider outside fallback-providers was used: %v", got)
	}
}

func TestModelFallbackIgnoresOtherErrorsAndDisabledMode(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		enabled bool
	}{
		{"server error", http.StatusInternalServerError, true},
		{"bad request", http.StatusBadRequest, true},
		{"disabled", http.StatusTooManyRequests, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newFallbackTestSetup(t, tc.status, 0, tc.enabled)
			_, _, errMsg := s.handler.ExecuteWithAuthManager(context.Background(), "openai", "fbtest-main-model", []byte(`{"model":"fbtest-main-model"}`), "")
			if errMsg == nil || errMsg.StatusCode != tc.status {
				t.Fatalf("errMsg = %+v, want status %d", errMsg, tc.status)
			}
			if got := s.pooled.Models(); len(got) != 0 {
				t.Fatalf("fallback must not run, got %v", got)
			}
		})
	}
}

func TestModelFallbackStreamReroutes(t *testing.T) {
	for _, tc := range []struct {
		name         string
		status       int
		streamStatus int
	}{
		{"execute error", http.StatusTooManyRequests, 0},
		{"first chunk error", 0, http.StatusTooManyRequests},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newFallbackTestSetup(t, tc.status, tc.streamStatus, true)
			dataChan, _, errChan := s.handler.ExecuteStreamWithAuthManager(context.Background(), "openai", "fbtest-main-model", []byte(`{"model":"fbtest-main-model"}`), "")
			var got []byte
			for chunk := range dataChan {
				got = append(got, chunk...)
			}
			for msg := range errChan {
				if msg != nil {
					t.Fatalf("unexpected error: %+v", msg)
				}
			}
			if string(got) != "fbtest-pooled" {
				t.Fatalf("stream = %q, want fallback provider response", got)
			}
			if models := s.pooled.Models(); len(models) != 1 || models[0] != "fbtest-fallback-model(high)" {
				t.Fatalf("fallback models = %v", models)
			}
		})
	}
}
