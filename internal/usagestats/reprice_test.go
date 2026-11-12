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

func TestRepriceTimelineTiersHourAndDayBuckets(t *testing.T) {
	monday := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	tr := newTestTracker(t.TempDir(), 0, monday.Add(20*24*time.Hour))
	ctx := context.Background()
	const key = "sk-alpha-0123456789"
	tr.Record(ctx, attemptRecord(key, "x", "flash", monday.Add(2*time.Hour)))   // peak hour
	tr.Record(ctx, attemptRecord(key, "x", "flash", monday.Add(5*time.Hour)))   // off-peak hour
	tr.Record(ctx, attemptRecord(key, "x", "flash", monday.Add(-24*time.Hour))) // Sunday, compacted below

	// Compact the Sunday bucket into a day bucket by hand.
	tr.mu.Lock()
	for k, c := range tr.buckets {
		if k.start.Before(monday) {
			delete(tr.buckets, k)
			k.span = bucketDay
			k.start = k.start.Truncate(24 * time.Hour)
			tr.buckets[k] = c
		}
	}
	tr.mu.Unlock()

	start, end := 100, 400
	peak := []ConditionalPrice{{UTCDays: []string{"monday"}, UTCStart: &start, UTCEnd: &end, Input: 3, Output: 15, CacheRead: 0.3, CacheWrite: 3.75}}
	req := RepriceRequest{Timeline: map[string][]PricePoint{"vendor/flash": {
		{From: monday.Add(-72 * time.Hour), Input: 1, Output: 5, CacheRead: 0.1, CacheWrite: 1.25, Conditional: peak},
	}}}
	current := fakePrices{{provider: "x", model: "flash"}: {Source: "openrouter", MatchedID: "vendor/flash"}}
	report, err := tr.RepriceUnpriced(req, current)
	// Peak hour at high (6975), off-peak hour at low (2325), Sunday never peaks (2325).
	if err != nil || report.Frozen != 3 || !approxEqual(report.AfterUSD, (6975.0+2325+2325)/1e6) {
		t.Fatalf("report = %+v, %v", report, err)
	}

	// A day bucket on Monday blends 3 peak hours with 21 off-peak hours.
	day := bucketKey{start: monday, span: bucketDay}
	c := counters{UncachedInputTokens: 1000, CacheReadTokens: 2000, CacheWriteTokens: 500, OutputTokens: 100}
	got := costOverSpan(c, Price{Input: 1, Output: 5, CacheRead: 0.1, CacheWrite: 1.25, Conditional: peak}, day)
	if want := (3*6975.0 + 21*2325.0) / 24 / 1e6; !approxEqual(got, want) {
		t.Fatalf("day blend = %v, want %v", got, want)
	}
}
