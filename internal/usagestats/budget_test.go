package usagestats

import (
	"context"
	"math"
	"testing"
	"time"
	_ "time/tzdata" // keep the time zone tests independent of the host zoneinfo

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// budgetPrices prices claude-sonnet-4-5 at $3/$15 per 1M tokens and leaves every
// other model unpriced.
var budgetPrices = fakePrices{
	{provider: "claude", model: "claude-sonnet-4-5"}: {
		Source:     "openrouter",
		MatchedID:  "anthropic/claude-sonnet-4.5",
		Input:      3,
		Output:     15,
		CacheRead:  0.3,
		CacheWrite: 3.75,
	},
}

// attemptCost is the cost of one attemptRecord for claude-sonnet-4-5 with
// budgetPrices (1000 uncached input, 2000 cache read, 500 cache write, 100 output).
const attemptCost = 6975.0 / 1e6

func TestBudgetWindowBounds(t *testing.T) {
	kolkata, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	// Saturday 2026-10-18 13:30 UTC == 19:00 IST.
	now := testNow
	for _, tc := range []struct {
		name       string
		loc        *time.Location
		period     string
		wantStart  time.Time
		wantResets time.Time
	}{
		{
			name:       "day UTC",
			loc:        time.UTC,
			period:     budgetPeriodDay,
			wantStart:  time.Date(2026, time.October, 18, 0, 0, 0, 0, time.UTC),
			wantResets: time.Date(2026, time.October, 19, 0, 0, 0, 0, time.UTC),
		},
		{
			name:       "day Kolkata",
			loc:        kolkata,
			period:     budgetPeriodDay,
			wantStart:  time.Date(2026, time.October, 18, 0, 0, 0, 0, kolkata),
			wantResets: time.Date(2026, time.October, 19, 0, 0, 0, 0, kolkata),
		},
		{
			name:       "week UTC starts Monday",
			loc:        time.UTC,
			period:     budgetPeriodWeek,
			wantStart:  time.Date(2026, time.October, 12, 0, 0, 0, 0, time.UTC),
			wantResets: time.Date(2026, time.October, 19, 0, 0, 0, 0, time.UTC),
		},
		{
			name:       "week Kolkata starts local Monday",
			loc:        kolkata,
			period:     budgetPeriodWeek,
			wantStart:  time.Date(2026, time.October, 12, 0, 0, 0, 0, kolkata),
			wantResets: time.Date(2026, time.October, 19, 0, 0, 0, 0, kolkata),
		},
		{
			name:       "month UTC",
			loc:        time.UTC,
			period:     budgetPeriodMonth,
			wantStart:  time.Date(2026, time.October, 1, 0, 0, 0, 0, time.UTC),
			wantResets: time.Date(2026, time.November, 1, 0, 0, 0, 0, time.UTC),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start, resets := budgetWindowBounds(now, tc.loc, tc.period)
			if !start.Equal(tc.wantStart) {
				t.Fatalf("start = %s, want %s", start, tc.wantStart)
			}
			if !resets.Equal(tc.wantResets) {
				t.Fatalf("resets = %s, want %s", resets, tc.wantResets)
			}
		})
	}
}

func TestBudgetWindowBoundsOnMondayAndMonthStart(t *testing.T) {
	// Monday 2026-10-12 00:05 UTC and Sunday 2026-11-01 23:59 UTC.
	monday := time.Date(2026, time.October, 12, 0, 5, 0, 0, time.UTC)
	if start, _ := budgetWindowBounds(monday, time.UTC, budgetPeriodWeek); !start.Equal(monday.Truncate(24 * time.Hour)) {
		t.Fatalf("Monday week start = %s, want the same Monday", start)
	}
	sunday := time.Date(2026, time.October, 18, 23, 59, 0, 0, time.UTC)
	if start, _ := budgetWindowBounds(sunday, time.UTC, budgetPeriodWeek); !start.Equal(time.Date(2026, time.October, 12, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("Sunday week start = %s, want the previous Monday", start)
	}
	monthStart := time.Date(2026, time.November, 1, 0, 0, 0, 0, time.UTC)
	start, resets := budgetWindowBounds(monthStart, time.UTC, budgetPeriodMonth)
	if !start.Equal(monthStart) || !resets.Equal(time.Date(2026, time.December, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("month bounds = %s/%s", start, resets)
	}
}

func TestKeySpendSinceSumsOnlyTheWindow(t *testing.T) {
	tr := newTestTracker(t.TempDir(), 0, testNow)
	tr.SetPricing(budgetPrices)
	ctx := context.Background()
	const key = "sk-budget-0123456789"
	const other = "sk-other-0123456789"
	keyID := KeyIDForRaw(key)

	dayStart := time.Date(2026, time.October, 18, 0, 0, 0, 0, time.UTC)
	tr.Record(ctx, attemptRecord(key, "claude", "claude-sonnet-4-5", dayStart.Add(-time.Hour)))
	tr.Record(ctx, attemptRecord(key, "claude", "claude-sonnet-4-5", dayStart.Add(time.Hour)))
	tr.Record(ctx, attemptRecord(key, "claude", "claude-sonnet-4-5", testNow.Add(-time.Minute)))
	tr.Record(ctx, attemptRecord(other, "claude", "claude-sonnet-4-5", testNow.Add(-time.Minute)))
	tr.Record(ctx, attemptRecord(key, "claude", "unpriced-model", testNow.Add(-time.Minute)))

	got := tr.KeySpendSince(keyID, dayStart)
	if want := 2 * attemptCost; math.Abs(got-want) > 1e-12 {
		t.Fatalf("KeySpendSince = %v, want %v", got, want)
	}
	// Buckets are hourly, so a window starting inside the current hour already
	// includes that hour's bucket while an earlier hour is excluded.
	if got := tr.KeySpendSince(keyID, testNow.Truncate(time.Hour)); math.Abs(got-attemptCost) > 1e-12 {
		t.Fatalf("KeySpendSince(current hour) = %v, want %v", got, attemptCost)
	}
	if got := tr.KeySpendSince(keyID, dayStart.Add(-6*time.Hour)); math.Abs(got-3*attemptCost) > 1e-12 {
		t.Fatalf("KeySpendSince(previous day) = %v, want %v", got, 3*attemptCost)
	}
	if got := tr.KeySpendSince("k_unknown", dayStart); got != 0 {
		t.Fatalf("unknown key spend = %v", got)
	}
	if got := KeyIDForRaw(""); got != UnattributedKeyID {
		t.Fatalf("KeyIDForRaw(\"\") = %q", got)
	}
}

func TestKeyBudgetReportsWindowsAndExceeded(t *testing.T) {
	tr := newTestTracker(t.TempDir(), 0, testNow)
	tr.SetPricing(budgetPrices)
	ctx := context.Background()
	const key = "sk-budget-0123456789"
	tr.Record(ctx, attemptRecord(key, "claude", "claude-sonnet-4-5", testNow.Add(-time.Minute)))

	limit := config.APIKeyLimit{DailyUSD: 0.005, WeeklyUSD: 0, MonthlyUSD: 60, Timezone: "Asia/Kolkata"}
	status := tr.KeyBudget(key, limit, testNow)
	if status.KeyID != KeyIDForRaw(key) {
		t.Fatalf("key id = %q", status.KeyID)
	}
	if len(status.Windows) != 2 {
		t.Fatalf("windows = %v, want day and month only", status.Windows)
	}
	day := status.Windows[0]
	if day.Period != budgetPeriodDay || day.LimitUSD != 0.005 || !day.Exceeded {
		t.Fatalf("day window = %+v", day)
	}
	if math.Abs(day.SpentUSD-attemptCost) > 1e-12 {
		t.Fatalf("day spend = %v, want %v", day.SpentUSD, attemptCost)
	}
	if !status.Exceeded {
		t.Fatal("status must report the exceeded window")
	}
	month := status.Windows[1]
	if month.Period != budgetPeriodMonth || month.Exceeded || month.LimitUSD != 60 {
		t.Fatalf("month window = %+v", month)
	}
	if month.ResetsAt.Location().String() != "Asia/Kolkata" {
		t.Fatalf("month resets location = %s", month.ResetsAt.Location())
	}

	if empty := tr.KeyBudget(key, config.APIKeyLimit{}, testNow); empty.Windows == nil || len(empty.Windows) != 0 || empty.Exceeded {
		t.Fatalf("limit without a cap = %+v", empty)
	}
	var nilTracker *Tracker
	if got := nilTracker.KeyBudget(key, limit, testNow); got.KeyID != KeyIDForRaw(key) || len(got.Windows) != 0 {
		t.Fatalf("nil tracker status = %+v", got)
	}
	if got := nilTracker.KeySpendSince(KeyIDForRaw(key), testNow); got != 0 {
		t.Fatalf("nil tracker spend = %v", got)
	}
}

func TestKeyBudgetCachesPerKeyAndLimit(t *testing.T) {
	tr := newTestTracker(t.TempDir(), 0, testNow)
	tr.SetPricing(budgetPrices)
	ctx := context.Background()
	const key = "sk-budget-0123456789"
	limit := config.APIKeyLimit{DailyUSD: 5}

	first := tr.KeyBudget(key, limit, testNow)
	if len(first.Windows) != 1 || first.Windows[0].SpentUSD != 0 {
		t.Fatalf("first status = %+v", first)
	}
	tr.Record(ctx, attemptRecord(key, "claude", "claude-sonnet-4-5", testNow.Add(-time.Minute)))

	// A second call within the TTL reuses the cached status.
	cached := tr.KeyBudget(key, limit, testNow.Add(budgetCacheTTL-time.Millisecond))
	if math.Abs(cached.Windows[0].SpentUSD-first.Windows[0].SpentUSD) > 1e-12 {
		t.Fatalf("expected the cached status, got %+v", cached)
	}

	// Once the TTL expires the fresh usage is picked up.
	fresh := tr.KeyBudget(key, limit, testNow.Add(budgetCacheTTL))
	if math.Abs(fresh.Windows[0].SpentUSD-attemptCost) > 1e-12 {
		t.Fatalf("expected a fresh status, got %+v", fresh)
	}

	// A different limit is never served from another limit's cache entry.
	tighter := tr.KeyBudget(key, config.APIKeyLimit{DailyUSD: 1}, testNow)
	if tighter.Windows[0].LimitUSD != 1 || math.Abs(tighter.Windows[0].SpentUSD-attemptCost) > 1e-12 {
		t.Fatalf("different limit used the cached status: %+v", tighter)
	}
}
