package helps

import (
	"testing"
	"time"
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
