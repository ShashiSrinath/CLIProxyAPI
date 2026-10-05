package executor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	cliproxysession "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

const openCodeGoTestBaseURL = "https://opencode.ai/zen/go/v1"

// openCodeGoFake serves chat and usage requests for the OpenCode Go base URL.
type openCodeGoFake struct {
	mu         sync.Mutex
	chatStatus int
	usageBody  string
	chat       []http.Header
	usageCalls int
}

func (f *openCodeGoFake) roundTrip(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	respond := func(status int, body string) (*http.Response, error) {
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    req,
		}, nil
	}
	switch req.URL.Path {
	case "/zen/go/v1/usage":
		f.usageCalls++
		return respond(http.StatusOK, f.usageBody)
	case "/zen/go/v1/chat/completions":
		f.chat = append(f.chat, req.Header.Clone())
		if f.chatStatus != http.StatusOK {
			return respond(f.chatStatus, `{"error":{"message":"usage limit reached"}}`)
		}
		if strings.Contains(req.Header.Get("Accept"), "text/event-stream") {
			return respond(http.StatusOK, "data: [DONE]\n\n")
		}
		return respond(http.StatusOK, `{"id":"chatcmpl-1","choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}
	return respond(http.StatusNotFound, `{}`)
}

func newOpenCodeGoTestAuth(extraAttrs map[string]string) *cliproxyauth.Auth {
	attrs := map[string]string{
		"base_url":     openCodeGoTestBaseURL,
		"api_key":      "test-key",
		"compat_name":  "opencode-go",
		"provider_key": "opencode-go",
	}
	for k, v := range extraAttrs {
		attrs[k] = v
	}
	return &cliproxyauth.Auth{Provider: "openai-compatibility", Attributes: attrs}
}

func runOpenCodeGo(t *testing.T, fake *openCodeGoFake, auth *cliproxyauth.Auth, headers http.Header, payload string, stream bool) error {
	t.Helper()
	cfg := &config.Config{
		OpenAICompatibility: []config.OpenAICompatibility{{
			Name:   "opencode-go",
			Models: []config.OpenAICompatibilityModel{{Name: "kimi-k2.6", Alias: "kimi-k2.6"}},
		}},
	}
	executor := NewOpenAICompatExecutor("openai-compatibility", cfg)
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", roundTripperFunc(fake.roundTrip))
	// Mirror the conductor, which enriches session metadata before executing.
	req, opts := cliproxysession.Enrich(cliproxyexecutor.Request{
		Model:   "kimi-k2.6",
		Payload: []byte(payload),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
		Headers:      headers,
		Stream:       stream,
	})
	if stream {
		result, err := executor.ExecuteStream(ctx, auth, req, opts)
		if err != nil {
			return err
		}
		for range result.Chunks {
		}
		return nil
	}
	_, err := executor.Execute(ctx, auth, req, opts)
	return err
}

func TestOpenAICompat_OpenCodeGoDefaultHeaders(t *testing.T) {
	turn1 := `{"model":"kimi-k2.6","messages":[{"role":"system","content":"sys"},{"role":"user","content":"first question"}]}`
	turn2 := `{"model":"kimi-k2.6","messages":[{"role":"system","content":"sys"},{"role":"user","content":"first question"},{"role":"assistant","content":"answer"},{"role":"user","content":"follow up"}]}`

	lastChat := func(t *testing.T, auth *cliproxyauth.Auth, headers http.Header, payload string) http.Header {
		t.Helper()
		fake := &openCodeGoFake{chatStatus: http.StatusOK}
		if err := runOpenCodeGo(t, fake, auth, headers, payload, false); err != nil {
			t.Fatalf("Execute error: %v", err)
		}
		if len(fake.chat) != 1 {
			t.Fatalf("expected 1 chat request, got %d", len(fake.chat))
		}
		return fake.chat[0]
	}

	t.Run("user agent and auth without header config", func(t *testing.T) {
		got := lastChat(t, newOpenCodeGoTestAuth(nil), http.Header{}, turn1)
		if ua := got.Get("User-Agent"); ua != util.OpenCodeGoUserAgent() {
			t.Fatalf("User-Agent = %q", ua)
		}
		if auth := got.Get("Authorization"); auth != "Bearer test-key" {
			t.Fatalf("Authorization = %q", auth)
		}
	})

	t.Run("configured headers override defaults", func(t *testing.T) {
		auth := newOpenCodeGoTestAuth(map[string]string{"header:User-Agent": "my-agent/2.0"})
		if ua := lastChat(t, auth, http.Header{}, turn1).Get("User-Agent"); ua != "my-agent/2.0" {
			t.Fatalf("User-Agent = %q", ua)
		}
	})

	t.Run("claude code session header", func(t *testing.T) {
		h := http.Header{"X-Claude-Code-Session-Id": {"11111111-2222-3333-4444-555555555555"}}
		got := lastChat(t, newOpenCodeGoTestAuth(nil), h, turn1).Get("X-Opencode-Session")
		if !strings.Contains(got, "11111111-2222-3333-4444-555555555555") {
			t.Fatalf("x-opencode-session = %q", got)
		}
	})

	t.Run("codex session header", func(t *testing.T) {
		h := http.Header{"Session_id": {"codex-thread-abc"}}
		got := lastChat(t, newOpenCodeGoTestAuth(nil), h, turn1).Get("X-Opencode-Session")
		if !strings.Contains(got, "codex-thread-abc") {
			t.Fatalf("x-opencode-session = %q", got)
		}
	})

	t.Run("headerless client gets stable derived session across turns", func(t *testing.T) {
		first := lastChat(t, newOpenCodeGoTestAuth(nil), http.Header{}, turn1).Get("X-Opencode-Session")
		second := lastChat(t, newOpenCodeGoTestAuth(nil), http.Header{}, turn2).Get("X-Opencode-Session")
		if first == "" || first != second {
			t.Fatalf("derived session not stable: %q -> %q", first, second)
		}
	})
}

func TestOpenAICompat_OpenCodeGoQuotaCooldown(t *testing.T) {
	payload := `{"model":"kimi-k2.6","messages":[{"role":"user","content":"hi"}]}`
	weeklyReset := time.Now().Add(3 * time.Hour).UTC().Format(time.RFC3339)
	exhausted := `{"usage":{"rolling":{"status":"ok","percent":40,"resetsAt":"2020-01-01T00:00:00.000Z"},` +
		`"weekly":{"status":"rate_limited","percent":100,"resetsAt":"` + weeklyReset + `"},` +
		`"monthly":{"status":"ok","percent":60,"resetsAt":"2099-01-01T00:00:00.000Z"}}}`
	healthy := `{"usage":{"rolling":{"status":"ok","percent":8,"resetsAt":"2099-01-01T00:00:00.000Z"}}}`

	for _, stream := range []bool{false, true} {
		name := "execute"
		if stream {
			name = "stream"
		}
		t.Run(name+" exhausted window blocks credential until reset", func(t *testing.T) {
			fake := &openCodeGoFake{chatStatus: http.StatusTooManyRequests, usageBody: exhausted}
			err := runOpenCodeGo(t, fake, newOpenCodeGoTestAuth(nil), http.Header{}, payload, stream)
			var se statusErr
			if !errors.As(err, &se) {
				t.Fatalf("expected statusErr, got %T %v", err, err)
			}
			if se.StatusCode() != http.StatusTooManyRequests || !se.IsCredentialScoped() {
				t.Fatalf("status=%d credentialScoped=%v", se.StatusCode(), se.IsCredentialScoped())
			}
			if se.RetryAfter() == nil || *se.RetryAfter() < 2*time.Hour || *se.RetryAfter() > 3*time.Hour {
				t.Fatalf("retryAfter = %v, want about 3h", se.RetryAfter())
			}
			if fake.usageCalls != 1 {
				t.Fatalf("usage calls = %d", fake.usageCalls)
			}
		})
	}

	t.Run("transient 429 keeps per-model backoff", func(t *testing.T) {
		fake := &openCodeGoFake{chatStatus: http.StatusTooManyRequests, usageBody: healthy}
		err := runOpenCodeGo(t, fake, newOpenCodeGoTestAuth(nil), http.Header{}, payload, false)
		var se statusErr
		if !errors.As(err, &se) {
			t.Fatalf("expected statusErr, got %T %v", err, err)
		}
		if se.IsCredentialScoped() || se.RetryAfter() != nil {
			t.Fatalf("credentialScoped=%v retryAfter=%v", se.IsCredentialScoped(), se.RetryAfter())
		}
	})

	t.Run("other errors skip usage lookup", func(t *testing.T) {
		fake := &openCodeGoFake{chatStatus: http.StatusInternalServerError, usageBody: exhausted}
		_ = runOpenCodeGo(t, fake, newOpenCodeGoTestAuth(nil), http.Header{}, payload, false)
		if fake.usageCalls != 0 {
			t.Fatalf("usage calls = %d", fake.usageCalls)
		}
	})
}
