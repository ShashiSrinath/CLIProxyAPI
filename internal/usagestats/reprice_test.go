package usagestats

import (
	"context"
	"net/url"
	"testing"
	"time"
)

func TestRepriceUnpricedUsesTimelineAndFreezes(t *testing.T) {
	dir := t.TempDir()
	tr := newTestTracker(dir, 0, testNow)
	ctx := context.Background()
	const key = "sk-alpha-0123456789"
	day1 := testNow.Add(-48 * time.Hour)
	day2 := testNow.Add(-time.Hour)
	tr.Record(ctx, attemptRecord(key, "claude", "flash", day1))
	tr.Record(ctx, attemptRecord(key, "claude", "flash", day2))
	tr.Record(ctx, attemptRecord(key, "claude", "other", day2))

	current := fakePrices{
		{provider: "claude", model: "flash"}: {Source: "openrouter", MatchedID: "vendor/flash", Input: 1, Output: 5, CacheRead: 0.1, CacheWrite: 1.25},
		{provider: "claude", model: "other"}: {Source: "openrouter", MatchedID: "vendor/other", Input: 1, Output: 5, CacheRead: 0.1, CacheWrite: 1.25},
	}
	req := RepriceRequest{
		Timeline: map[string][]PricePoint{"Vendor/Flash": {
			{From: day2.Truncate(24 * time.Hour), Input: 3, Output: 15, CacheRead: 0.3, CacheWrite: 3.75},
			{From: day1.Add(-time.Hour), Input: 2, Output: 10, CacheRead: 0.2, CacheWrite: 2.5},
		}},
		FreezeRest: true,
	}
	const low, mid, high = 2325.0 / 1e6, 4650.0 / 1e6, 6975.0 / 1e6

	dry := req
	dry.DryRun = true
	report, err := tr.RepriceUnpriced(dry, current)
	if err != nil || report.Frozen != 0 || !approxEqual(report.AfterUSD, mid+high+low) {
		t.Fatalf("dry run = %+v, %v", report, err)
	}

	report, err = tr.RepriceUnpriced(req, current)
	if err != nil || report.Frozen != 3 || !approxEqual(report.BeforeUSD, 3*low) || !approxEqual(report.AfterUSD, mid+high+low) {
		t.Fatalf("reprice = %+v, %v", report, err)
	}

	// A later price change no longer moves the frozen history, also after reload.
	cheaper := fakePrices{}
	q := mustQuery(t, url.Values{"range": {"7d"}}, testNow)
	reloaded := newTestTracker(dir, 0, testNow)
	if err := reloaded.load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := reloaded.Summary(q, nil, cheaper).Totals.CostUSD; !approxEqual(got, mid+high+low) {
		t.Fatalf("frozen cost = %v", got)
	}
	again, _ := reloaded.RepriceUnpriced(req, current)
	if again.Frozen != 0 {
		t.Fatalf("second reprice froze %d buckets", again.Frozen)
	}
}
