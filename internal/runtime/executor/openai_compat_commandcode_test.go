package executor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	cliproxysession "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

const commandCodeTestBaseURL = "https://api.commandcode.ai/provider/v1"

// commandCodeFake serves chat and credits requests for the Command Code base URL.
type commandCodeFake struct {
	mu                 sync.Mutex
	chatStatus         int
	chatErrorBody      string
	usageBody          string
	usageCalls         int
	usageHeader        http.Header
	subscriptionStatus int
	subscriptionBody   string
	subscriptionCalls  int
	subscriptionHeader http.Header
}

func (f *commandCodeFake) roundTrip(req *http.Request) (*http.Response, error) {
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
	case "/alpha/billing/credits":
		f.usageCalls++
		f.usageHeader = req.Header.Clone()
		return respond(http.StatusOK, f.usageBody)
	case "/alpha/billing/subscriptions":
		f.subscriptionCalls++
		f.subscriptionHeader = req.Header.Clone()
		status := f.subscriptionStatus
		if status == 0 {
			status = http.StatusOK
		}
		return respond(status, f.subscriptionBody)
	case "/provider/v1/chat/completions":
		if f.chatStatus != http.StatusOK {
			body := f.chatErrorBody
			if body == "" {
				body = `{"error":{"message":"rate limited","type":"rate_limit_error"}}`
			}
			return respond(f.chatStatus, body)
		}
		if strings.Contains(req.Header.Get("Accept"), "text/event-stream") {
			return respond(http.StatusOK, "data: [DONE]\n\n")
		}
		return respond(http.StatusOK, `{"id":"chatcmpl-1","choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}
	return respond(http.StatusNotFound, `{}`)
}

func newCommandCodeTestAuth(extraAttrs map[string]string) *cliproxyauth.Auth {
	attrs := map[string]string{
		"base_url":     commandCodeTestBaseURL,
		"api_key":      "test-key",
		"compat_name":  "commandcode",
		"provider_key": "commandcode",
	}
	for k, v := range extraAttrs {
		attrs[k] = v
	}
	return &cliproxyauth.Auth{Provider: "openai-compatibility", Attributes: attrs}
}

func runCommandCode(t *testing.T, fake *commandCodeFake, auth *cliproxyauth.Auth, payload string, stream bool) error {
	t.Helper()
	cfg := &config.Config{
		OpenAICompatibility: []config.OpenAICompatibility{{
			Name:   "commandcode",
			Models: []config.OpenAICompatibilityModel{{Name: "deepseek/deepseek-v4.1-flash", Alias: "deepseek/deepseek-v4.1-flash"}},
		}},
	}
	executor := NewOpenAICompatExecutor("openai-compatibility", cfg)
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", roundTripperFunc(fake.roundTrip))
	req, opts := cliproxysession.Enrich(cliproxyexecutor.Request{
		Model:   "deepseek/deepseek-v4.1-flash",
		Payload: []byte(payload),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
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

func TestOpenAICompat_CommandCodeQuotaCooldown(t *testing.T) {
	payload := `{"model":"deepseek/deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`
	weeklyReset := strconv.FormatInt(time.Now().Add(3*time.Hour).UnixMilli(), 10)
	exhausted := `{"windowLimits":{"fiveHour":{"used":0.08,"cap":14,"exceeded":false,"resetAt":1},` +
		`"weekly":{"used":35,"cap":35,"exceeded":true,"resetAt":` + weeklyReset + `}}}`
	healthy := `{"windowLimits":{"fiveHour":{"used":0.08,"cap":14,"exceeded":false,"resetAt":` + weeklyReset + `}}}`

	for _, stream := range []bool{false, true} {
		name := "execute"
		if stream {
			name = "stream"
		}
		t.Run(name+" exhausted window blocks credential until reset", func(t *testing.T) {
			fake := &commandCodeFake{chatStatus: http.StatusTooManyRequests, usageBody: exhausted}
			err := runCommandCode(t, fake, newCommandCodeTestAuth(nil), payload, stream)
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
			if got := fake.usageHeader.Get("Authorization"); got != "Bearer test-key" {
				t.Fatalf("usage Authorization = %q", got)
			}
		})
	}

	t.Run("transient 429 keeps per-model backoff", func(t *testing.T) {
		fake := &commandCodeFake{chatStatus: http.StatusTooManyRequests, usageBody: healthy}
		err := runCommandCode(t, fake, newCommandCodeTestAuth(nil), payload, false)
		var se statusErr
		if !errors.As(err, &se) {
			t.Fatalf("expected statusErr, got %T %v", err, err)
		}
		if se.IsCredentialScoped() || se.RetryAfter() != nil {
			t.Fatalf("credentialScoped=%v retryAfter=%v", se.IsCredentialScoped(), se.RetryAfter())
		}
	})

	t.Run("other errors skip usage lookup", func(t *testing.T) {
		fake := &commandCodeFake{chatStatus: http.StatusInternalServerError, usageBody: exhausted}
		_ = runCommandCode(t, fake, newCommandCodeTestAuth(nil), payload, false)
		if fake.usageCalls != 0 {
			t.Fatalf("usage calls = %d", fake.usageCalls)
		}
	})
}

func TestOpenAICompat_CommandCodeInsufficientCredits(t *testing.T) {
	payload := `{"model":"deepseek/deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`
	insufficient := `{"error":{"message":"You have insufficient credits to make this request. Please purchase more credits to continue using the service.","type":"invalid_request_error","code":"BAD_REQUEST"}}`
	periodEnd := func(d time.Duration) string {
		return `{"success":true,"data":{"status":"active","currentPeriodEnd":"` + time.Now().Add(d).UTC().Format(time.RFC3339Nano) + `"}}`
	}
	requireCreditsErr := func(t *testing.T, err error) statusErr {
		t.Helper()
		var se statusErr
		if !errors.As(err, &se) {
			t.Fatalf("expected statusErr, got %T %v", err, err)
		}
		if se.StatusCode() != http.StatusTooManyRequests || !se.IsCredentialScoped() {
			t.Fatalf("status=%d credentialScoped=%v", se.StatusCode(), se.IsCredentialScoped())
		}
		if !strings.Contains(se.Error(), "insufficient credits") || !strings.Contains(se.Error(), `"insufficient_quota"`) {
			t.Fatalf("client-visible error lost its meaning: %s", se.Error())
		}
		if se.RetryAfter() == nil {
			t.Fatalf("retryAfter missing")
		}
		return se
	}

	for _, stream := range []bool{false, true} {
		name := "execute"
		if stream {
			name = "stream"
		}
		for _, status := range []int{http.StatusBadRequest, http.StatusPaymentRequired} {
			t.Run(name+" "+strconv.Itoa(status)+" cools credential until near refill", func(t *testing.T) {
				fake := &commandCodeFake{chatStatus: status, chatErrorBody: insufficient, subscriptionBody: periodEnd(2 * time.Hour)}
				se := requireCreditsErr(t, runCommandCode(t, fake, newCommandCodeTestAuth(nil), payload, stream))
				if got := *se.RetryAfter(); got < 110*time.Minute || got > 2*time.Hour {
					t.Fatalf("retryAfter = %v, want about 2h", got)
				}
				if fake.subscriptionCalls != 1 || fake.usageCalls != 0 {
					t.Fatalf("subscription calls = %d usage calls = %d", fake.subscriptionCalls, fake.usageCalls)
				}
				if got := fake.subscriptionHeader.Get("Authorization"); got != "Bearer test-key" {
					t.Fatalf("subscription Authorization = %q", got)
				}
			})
		}
	}

	t.Run("distant refill is capped", func(t *testing.T) {
		fake := &commandCodeFake{chatStatus: http.StatusBadRequest, chatErrorBody: insufficient, subscriptionBody: periodEnd(15 * 24 * time.Hour)}
		se := requireCreditsErr(t, runCommandCode(t, fake, newCommandCodeTestAuth(nil), payload, false))
		if got := *se.RetryAfter(); got != helps.CommandCodeCreditsMaxCooldown {
			t.Fatalf("retryAfter = %v, want %v", got, helps.CommandCodeCreditsMaxCooldown)
		}
	})

	t.Run("subscription lookup failure uses fallback cooldown", func(t *testing.T) {
		fake := &commandCodeFake{chatStatus: http.StatusBadRequest, chatErrorBody: insufficient, subscriptionStatus: http.StatusInternalServerError, subscriptionBody: `{}`}
		se := requireCreditsErr(t, runCommandCode(t, fake, newCommandCodeTestAuth(nil), payload, false))
		if got := *se.RetryAfter(); got != helps.CommandCodeCreditsFallbackCooldown {
			t.Fatalf("retryAfter = %v, want %v", got, helps.CommandCodeCreditsFallbackCooldown)
		}
	})

	t.Run("past period end uses fallback cooldown", func(t *testing.T) {
		fake := &commandCodeFake{chatStatus: http.StatusBadRequest, chatErrorBody: insufficient, subscriptionBody: periodEnd(-time.Hour)}
		se := requireCreditsErr(t, runCommandCode(t, fake, newCommandCodeTestAuth(nil), payload, false))
		if got := *se.RetryAfter(); got != helps.CommandCodeCreditsFallbackCooldown {
			t.Fatalf("retryAfter = %v, want %v", got, helps.CommandCodeCreditsFallbackCooldown)
		}
	})

	t.Run("ordinary bad request stays a request error", func(t *testing.T) {
		fake := &commandCodeFake{chatStatus: http.StatusBadRequest, chatErrorBody: `{"error":{"message":"messages: field required","type":"invalid_request_error","code":"BAD_REQUEST"}}`}
		err := runCommandCode(t, fake, newCommandCodeTestAuth(nil), payload, false)
		var se statusErr
		if !errors.As(err, &se) {
			t.Fatalf("expected statusErr, got %T %v", err, err)
		}
		if se.StatusCode() != http.StatusBadRequest || se.IsCredentialScoped() || se.RetryAfter() != nil {
			t.Fatalf("status=%d credentialScoped=%v retryAfter=%v", se.StatusCode(), se.IsCredentialScoped(), se.RetryAfter())
		}
		if fake.subscriptionCalls != 0 {
			t.Fatalf("subscription calls = %d", fake.subscriptionCalls)
		}
	})
}
