package helps

import (
	"testing"
	"time"
)

func TestParseOpenCodeGoUsage(t *testing.T) {
	// Captured from https://opencode.ai/zen/go/v1/usage.
	body := []byte(`{"usage":{"rolling":{"status":"ok","percent":8,"resetsAt":"2026-10-05T02:09:54.000Z"},"weekly":{"status":"ok","percent":2,"resetsAt":"2026-10-12T00:00:00.000Z"},"monthly":{"status":"ok","percent":12,"resetsAt":"2026-11-04T09:01:38.000Z"}}}`)
	windows := ParseOpenCodeGoUsage(body)
	if len(windows) != 3 {
		t.Fatalf("windows = %d", len(windows))
	}
	if windows[0].Key != "rolling" || windows[0].Percent != 8 || windows[0].ResetsAt.IsZero() {
		t.Fatalf("rolling = %+v", windows[0])
	}
	if _, ok := OpenCodeGoQuotaCooldown(windows, time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)); ok {
		t.Fatalf("healthy windows must not produce a cooldown")
	}
	if ParseOpenCodeGoUsage([]byte(`{"error":"nope"}`)) != nil {
		t.Fatalf("expected nil for unexpected shape")
	}
}

func TestOpenCodeGoQuotaCooldown(t *testing.T) {
	now := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	windows := []OpenCodeGoUsageWindow{
		{Key: "rolling", Status: "rate_limited", Percent: 100, ResetsAt: now.Add(2 * time.Hour)},
		{Key: "weekly", Status: "ok", Percent: 100, ResetsAt: now.Add(48 * time.Hour)},
		{Key: "monthly", Status: "ok", Percent: 50, ResetsAt: now.Add(400 * time.Hour)},
	}
	got, ok := OpenCodeGoQuotaCooldown(windows, now)
	if !ok || got != 48*time.Hour {
		t.Fatalf("cooldown = %v ok=%v, want 48h (latest exhausted window)", got, ok)
	}

	stale := []OpenCodeGoUsageWindow{{Key: "rolling", Percent: 100, ResetsAt: now.Add(-time.Minute)}}
	if _, ok := OpenCodeGoQuotaCooldown(stale, now); ok {
		t.Fatalf("past reset must not produce a cooldown")
	}
}
