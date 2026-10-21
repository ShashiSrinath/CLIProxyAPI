package helps

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tidwall/gjson"
)

func TestParseCommandCodeUsage(t *testing.T) {
	// Captured from https://api.commandcode.ai/alpha/billing/credits.
	body := []byte(`{"credits":{"monthlyCredits":69.9177663613,"purchasedCredits":0,"freeCredits":0},"windowLimits":{"limited":true,"exceeded":null,"fiveHour":{"used":0.0822336387,"cap":14,"exceeded":false,"resetAt":1791256488371},"weekly":{"used":0.0822336387,"cap":35,"exceeded":false,"resetAt":1791843288371}},"sandboxAccess":false}`)
	windows := ParseCommandCodeUsage(body)
	if len(windows) != 2 {
		t.Fatalf("windows = %d", len(windows))
	}
	if windows[0].Key != "fiveHour" || windows[0].Cap != 14 || windows[0].Used != 0.0822336387 || windows[0].ResetsAt.IsZero() {
		t.Fatalf("fiveHour = %+v", windows[0])
	}
	if windows[1].Key != "weekly" || windows[1].Cap != 35 {
		t.Fatalf("weekly = %+v", windows[1])
	}
	if _, ok := CommandCodeQuotaCooldown(windows, time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)); ok {
		t.Fatalf("healthy windows must not produce a cooldown")
	}
	if ParseCommandCodeUsage([]byte(`{"error":"nope"}`)) != nil {
		t.Fatalf("expected nil for unexpected shape")
	}
}

func TestCommandCodeQuotaCooldown(t *testing.T) {
	now := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	windows := []CommandCodeUsageWindow{
		{Key: "fiveHour", Used: 14, Cap: 14, Exceeded: true, ResetsAt: now.Add(2 * time.Hour)},
		{Key: "weekly", Used: 35, Cap: 35, Exceeded: false, ResetsAt: now.Add(48 * time.Hour)},
	}
	got, ok := CommandCodeQuotaCooldown(windows, now)
	if !ok || got != 48*time.Hour {
		t.Fatalf("cooldown = %v ok=%v, want 48h (latest exhausted window)", got, ok)
	}

	// used >= cap is exhausted even without the upstream exceeded flag.
	usedOut := []CommandCodeUsageWindow{{Key: "fiveHour", Used: 14, Cap: 14, ResetsAt: now.Add(time.Hour)}}
	if _, ok := CommandCodeQuotaCooldown(usedOut, now); !ok {
		t.Fatalf("used >= cap must count as exhausted")
	}

	stale := []CommandCodeUsageWindow{{Key: "fiveHour", Used: 14, Cap: 14, ResetsAt: now.Add(-time.Minute)}}
	if _, ok := CommandCodeQuotaCooldown(stale, now); ok {
		t.Fatalf("past reset must not produce a cooldown")
	}
}

func TestIsCommandCodeInsufficientCredits(t *testing.T) {
	// Captured from https://api.commandcode.ai/provider/v1 with a ~$0.06 balance.
	insufficient := []byte(`{"error":{"message":"You have insufficient credits to make this request. Please purchase more credits to continue using the service.","type":"invalid_request_error","code":"BAD_REQUEST"}}`)
	tests := []struct {
		name   string
		status int
		body   []byte
		want   bool
	}{
		{name: "400 insufficient credits", status: http.StatusBadRequest, body: insufficient, want: true},
		{name: "402 insufficient credits", status: http.StatusPaymentRequired, body: insufficient, want: true},
		{name: "top-level message, mixed case", status: http.StatusBadRequest, body: []byte(`{"message":"Insufficient Credits"}`), want: true},
		{name: "ordinary bad request", status: http.StatusBadRequest, body: []byte(`{"error":{"message":"messages: field required","type":"invalid_request_error","code":"BAD_REQUEST"}}`), want: false},
		{name: "other status", status: http.StatusTooManyRequests, body: insufficient, want: false},
		{name: "server error", status: http.StatusInternalServerError, body: insufficient, want: false},
		{name: "phrase outside message", status: http.StatusBadRequest, body: []byte(`{"error":{"message":"invalid input","param":"insufficient credits"}}`), want: false},
		{name: "plain text body", status: http.StatusBadRequest, body: []byte(`insufficient credits`), want: false},
		{name: "empty body", status: http.StatusBadRequest, body: nil, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := IsCommandCodeInsufficientCredits(test.status, test.body); got != test.want {
				t.Fatalf("IsCommandCodeInsufficientCredits() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestCommandCodeInsufficientCreditsBody(t *testing.T) {
	body := CommandCodeInsufficientCreditsBody([]byte(`{"error":{"message":"You have insufficient credits to make this request.","type":"invalid_request_error","code":"BAD_REQUEST"}}`))
	if got := gjson.GetBytes(body, "error.message").String(); got != "You have insufficient credits to make this request." {
		t.Fatalf("message = %q", got)
	}
	if gjson.GetBytes(body, "error.type").String() != "insufficient_quota" || gjson.GetBytes(body, "error.code").String() != "insufficient_quota" {
		t.Fatalf("body = %s", body)
	}
}

func TestParseCommandCodeSubscriptionPeriodEnd(t *testing.T) {
	// Captured from https://api.commandcode.ai/alpha/billing/subscriptions.
	body := []byte(`{"success":true,"data":{"status":"active","currentPeriodStart":"2026-10-05T22:08:39.000Z","currentPeriodEnd":"2026-11-05T22:08:39.000Z","cancelAtPeriodEnd":false,"planId":"individual-goat"}}`)
	got, ok := ParseCommandCodeSubscriptionPeriodEnd(body)
	if want := time.Date(2026, 11, 5, 22, 8, 39, 0, time.UTC); !ok || !got.Equal(want) {
		t.Fatalf("period end = %v ok=%v, want %v", got, ok, want)
	}
	for _, bad := range []string{`{"success":false}`, `{"data":{"currentPeriodEnd":"soon"}}`, `not json`} {
		if _, ok := ParseCommandCodeSubscriptionPeriodEnd([]byte(bad)); ok {
			t.Fatalf("expected parse failure for %s", bad)
		}
	}
}

func TestCommandCodeCreditsCooldown(t *testing.T) {
	now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		periodEnd time.Time
		want      time.Duration
	}{
		{name: "refill soon", periodEnd: now.Add(2 * time.Hour), want: 2 * time.Hour},
		{name: "refill far away is capped", periodEnd: now.Add(15 * 24 * time.Hour), want: CommandCodeCreditsMaxCooldown},
		{name: "unknown period end", want: CommandCodeCreditsFallbackCooldown},
		{name: "period end in the past", periodEnd: now.Add(-time.Minute), want: CommandCodeCreditsFallbackCooldown},
		{name: "period end now", periodEnd: now, want: CommandCodeCreditsFallbackCooldown},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := CommandCodeCreditsCooldown(test.periodEnd, now); got != test.want {
				t.Fatalf("cooldown = %v, want %v", got, test.want)
			}
		})
	}
}

func TestFetchCommandCodeSubscriptionPeriodEnd(t *testing.T) {
	var gotPath, gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"success":true,"data":{"currentPeriodEnd":"2026-11-05T22:08:39.000Z"}}`))
	}))
	defer server.Close()

	got, errFetch := FetchCommandCodeSubscriptionPeriodEnd(context.Background(), server.Client(), server.URL+"/provider/v1", "test-key")
	if errFetch != nil {
		t.Fatalf("fetch error: %v", errFetch)
	}
	if !got.Equal(time.Date(2026, 11, 5, 22, 8, 39, 0, time.UTC)) {
		t.Fatalf("period end = %v", got)
	}
	if gotPath != "/alpha/billing/subscriptions" || gotAuth != "Bearer test-key" {
		t.Fatalf("path=%q auth=%q", gotPath, gotAuth)
	}

	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer failing.Close()
	if _, errFetch = FetchCommandCodeSubscriptionPeriodEnd(context.Background(), failing.Client(), failing.URL, "test-key"); errFetch == nil {
		t.Fatalf("expected error for non-2xx status")
	}
}
