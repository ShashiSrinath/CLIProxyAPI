package usagestats

import (
	"context"
	"encoding/json"
	"math"
	"net/url"
	"strings"
	"testing"
	"time"
	_ "time/tzdata" // keep the time zone tests independent of the host zoneinfo

	internallogging "github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
)

// fakePrices resolves prices from a fixed table; unknown models are unpriced.
type fakePrices map[modelID]Price

func (f fakePrices) Resolve(provider, model string) Price {
	if price, ok := f[modelID{provider: provider, model: model}]; ok {
		return price
	}
	return Price{Source: priceSourceNone}
}

func mustQuery(t *testing.T, values url.Values, now time.Time) SummaryQuery {
	t.Helper()
	q, err := ParseSummaryQuery(values, now)
	if err != nil {
		t.Fatalf("ParseSummaryQuery(%v) error = %v", values, err)
	}
	return q
}

func findKeyRow(t *testing.T, resp SummaryResponse, id string) KeyRow {
	t.Helper()
	for _, row := range resp.Keys {
		if row.ID == id {
			return row
		}
	}
	t.Fatalf("key row %q not found in %d rows", id, len(resp.Keys))
	return KeyRow{}
}

func findModelRow(t *testing.T, models []ModelRow, model string) ModelRow {
	t.Helper()
	for _, row := range models {
		if row.Model == model {
			return row
		}
	}
	t.Fatalf("model row %q not found", model)
	return ModelRow{}
}

func approxEqual(a, b float64) bool {
	return math.Abs(a-b) < 1e-9
}

func TestParseSummaryQueryDefaultsToLast24HoursInUTC(t *testing.T) {
	q := mustQuery(t, url.Values{}, testNow)
	if want := time.Date(2026, 10, 17, 14, 0, 0, 0, time.UTC); !q.From.Equal(want) {
		t.Fatalf("From = %v, want %v", q.From, want)
	}
	if want := time.Date(2026, 10, 18, 14, 0, 0, 0, time.UTC); !q.To.Equal(want) {
		t.Fatalf("To = %v, want %v (now rounded up to the next hour)", q.To, want)
	}
	if q.Bucket != bucketHour {
		t.Fatalf("Bucket = %q, want hour", q.Bucket)
	}
	if q.Location.String() != "UTC" {
		t.Fatalf("Location = %q, want UTC", q.Location)
	}

	onHour := time.Date(2026, 10, 18, 13, 0, 0, 0, time.UTC)
	if q := mustQuery(t, url.Values{}, onHour); !q.To.Equal(onHour) {
		t.Fatalf("To for an exact hour = %v, want %v", q.To, onHour)
	}
}

func TestParseSummaryQueryBucketDefaults(t *testing.T) {
	cases := map[string]struct {
		values     url.Values
		wantBucket string
	}{
		"24h defaults to hour": {url.Values{"range": {"24h"}}, bucketHour},
		"7d defaults to day":   {url.Values{"range": {"7d"}}, bucketDay},
		"explicit day":         {url.Values{"range": {"24h"}, "bucket": {"day"}}, bucketDay},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if q := mustQuery(t, tc.values, testNow); q.Bucket != tc.wantBucket {
				t.Fatalf("Bucket = %q, want %q", q.Bucket, tc.wantBucket)
			}
		})
	}
}

func TestParseSummaryQueryRejectsInvalidInput(t *testing.T) {
	cases := map[string]url.Values{
		"unknown range":         {"range": {"2w"}},
		"range with from":       {"range": {"7d"}, "from": {"2026-10-10T00:00:00Z"}},
		"to without from":       {"to": {"2026-10-18T00:00:00Z"}},
		"bad from":              {"from": {"yesterday"}},
		"to before from":        {"from": {"2026-10-18T00:00:00Z"}, "to": {"2026-10-17T00:00:00Z"}},
		"hour over 8 days":      {"from": {"2026-10-01T00:00:00Z"}, "to": {"2026-10-12T00:00:00Z"}, "bucket": {"hour"}},
		"90d with hour buckets": {"range": {"90d"}, "bucket": {"hour"}},
		"unknown bucket":        {"bucket": {"week"}},
		"unknown timezone":      {"tz": {"Mars/Olympus"}},
		"span beyond the limit": {"from": {"2024-01-01T00:00:00Z"}, "to": {"2026-10-18T00:00:00Z"}},
	}
	for name, values := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseSummaryQuery(values, testNow); err == nil {
				t.Fatalf("ParseSummaryQuery(%v) error = nil, want error", values)
			}
		})
	}

	accepted := map[string]url.Values{
		"day buckets over 8 days": {"from": {"2026-10-01T00:00:00Z"}, "to": {"2026-10-12T00:00:00Z"}, "bucket": {"day"}},
		"from without to":         {"from": {"2026-10-18T00:00:00Z"}},
		"known timezone":          {"tz": {"Asia/Kolkata"}},
	}
	for name, values := range accepted {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseSummaryQuery(values, testNow); err != nil {
				t.Fatalf("ParseSummaryQuery(%v) error = %v", values, err)
			}
		})
	}
}

func TestSummaryKeyIdentityAndMaskingRoundTrip(t *testing.T) {
	tr := newTestTracker(t.TempDir(), 0, testNow)
	ctx := context.Background()
	const laptop = "sk-abcdefghijklmnop"
	const stale = "sk-stale-key-99999"
	const idle = "sk-idle-key-000001"
	tr.Record(ctx, attemptRecord(laptop, "claude", "m", testNow.Add(-10*time.Minute)))
	tr.Record(ctx, attemptRecord(stale, "claude", "m", testNow.Add(-20*time.Minute)))
	tr.Record(ctx, attemptRecord("", "claude", "m", testNow.Add(-30*time.Minute)))

	keys := []ConfiguredKey{
		{Raw: laptop, Name: "Laptop", Providers: []string{"claude"}},
		{Raw: idle, Name: "CI"},
	}
	resp := tr.Summary(mustQuery(t, url.Values{"range": {"24h"}}, testNow), keys, nil)

	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal summary: %v", err)
	}
	for _, secret := range []string{laptop, stale, idle} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("summary leaks a raw client key")
		}
	}

	laptopRow := findKeyRow(t, resp, keyIDForRaw(laptop))
	if laptopRow.Name != "Laptop" || laptopRow.Masked != "sk-a…mnop" || !laptopRow.Configured {
		t.Fatalf("configured key row = %+v", laptopRow)
	}
	if len(laptopRow.Providers) != 1 || laptopRow.Providers[0] != "claude" {
		t.Fatalf("providers = %v, want [claude]", laptopRow.Providers)
	}
	if laptopRow.Totals.Requests != 1 || laptopRow.FirstSeen != "2026-10-18T13:20:00Z" || laptopRow.LastUsed != "2026-10-18T13:20:00Z" {
		t.Fatalf("laptop totals/timestamps = %+v first=%q last=%q", laptopRow.Totals, laptopRow.FirstSeen, laptopRow.LastUsed)
	}

	staleRow := findKeyRow(t, resp, keyIDForRaw(stale))
	if staleRow.Configured || staleRow.Name != "" || staleRow.Masked != "sk-s…9999" {
		t.Fatalf("removed key row = %+v, want unconfigured with masked form kept", staleRow)
	}
	if staleRow.Providers == nil || len(staleRow.Providers) != 0 {
		t.Fatalf("providers = %#v, want empty non-nil slice", staleRow.Providers)
	}

	unattributed := findKeyRow(t, resp, UnattributedKeyID)
	if unattributed.Masked != "" || unattributed.Totals.Requests != 1 {
		t.Fatalf("unattributed row = %+v", unattributed)
	}

	idleRow := findKeyRow(t, resp, keyIDForRaw(idle))
	if !idleRow.Configured || idleRow.Name != "CI" || idleRow.Masked != "sk-i…0001" {
		t.Fatalf("idle configured key row = %+v", idleRow)
	}
	if idleRow.Totals.Requests != 0 || len(idleRow.Series) != 24 || idleRow.Models == nil || len(idleRow.Models) != 0 {
		t.Fatalf("idle key should have zero totals, 24 zero-filled points and no models: %+v", idleRow)
	}
	if last := resp.Keys[len(resp.Keys)-1]; last.ID != idleRow.ID {
		t.Fatalf("zero-usage key should sort last, got %q", last.ID)
	}
}

func TestSummaryRequestsAttemptsFailedAndLatency(t *testing.T) {
	tr := newTestTracker(t.TempDir(), 0, testNow)
	ctx := context.Background()
	const key = "sk-alpha-0123456789"
	at := testNow.Add(-5 * time.Minute)

	req1 := internallogging.WithRequestID(ctx, "req-1")
	rec := attemptRecord(key, "claude", "m", at)
	tr.Record(req1, rec)
	tr.Record(req1, rec) // retry of the same inbound request

	failed := attemptRecord(key, "claude", "m", at)
	failed.Failed = true
	failed.Latency = 750 * time.Millisecond
	tr.Record(internallogging.WithRequestID(ctx, "req-2"), failed)

	resp := tr.Summary(mustQuery(t, url.Values{"range": {"24h"}}, testNow), nil, nil)
	if resp.Totals.Requests != 2 || resp.Totals.Attempts != 3 || resp.Totals.Failed != 1 {
		t.Fatalf("totals requests/attempts/failed = %d/%d/%d, want 2/3/1",
			resp.Totals.Requests, resp.Totals.Attempts, resp.Totals.Failed)
	}
	if want := 1250.0 / 3.0; !approxEqual(resp.Totals.AvgLatencyMs, want) {
		t.Fatalf("avg_latency_ms = %v, want %v (average over attempts)", resp.Totals.AvgLatencyMs, want)
	}
}

func TestSummaryCacheTokensAndCostMath(t *testing.T) {
	tr := newTestTracker(t.TempDir(), 0, testNow)
	ctx := context.Background()
	const key = "sk-alpha-0123456789"
	at := testNow.Add(-time.Minute)
	tr.Record(ctx, attemptRecord(key, "claude", "claude-sonnet-4-5", at))
	tr.Record(ctx, attemptRecord(key, "claude", "mystery-model", at))

	prices := fakePrices{
		{provider: "claude", model: "claude-sonnet-4-5"}: {
			Source:     "openrouter",
			MatchedID:  "anthropic/claude-sonnet-4.5",
			Input:      3,
			Output:     15,
			CacheRead:  0.3,
			CacheWrite: 3.75,
		},
	}
	resp := tr.Summary(mustQuery(t, url.Values{"range": {"24h"}}, testNow), nil, prices)

	// Per attempt: 1000 uncached*3 + 2000 read*0.3 + 500 write*3.75 + 100 output*15,
	// divided by 1e6. The unpriced model adds nothing.
	const wantCost = 6975.0 / 1e6
	if !approxEqual(resp.Totals.CostUSD, wantCost) {
		t.Fatalf("cost = %v, want %v", resp.Totals.CostUSD, wantCost)
	}
	if resp.Totals.UncachedInputTokens != 2000 || resp.Totals.CacheReadTokens != 4000 ||
		resp.Totals.CacheWriteTokens != 1000 || resp.Totals.InputTokens != 7000 ||
		resp.Totals.OutputTokens != 200 || resp.Totals.ReasoningTokens != 40 {
		t.Fatalf("token totals = %+v", resp.Totals)
	}

	sonnet := findModelRow(t, resp.Models, "claude-sonnet-4-5")
	if !sonnet.Priced || sonnet.Price.MatchedID != "anthropic/claude-sonnet-4.5" || sonnet.Price.CacheRead != 0.3 {
		t.Fatalf("priced model row = %+v", sonnet)
	}
	if !approxEqual(sonnet.Totals.CostUSD, wantCost) || len(sonnet.Keys) != 1 || !approxEqual(sonnet.Keys[0].CostUSD, wantCost) {
		t.Fatalf("priced model cost = %v keys = %+v", sonnet.Totals.CostUSD, sonnet.Keys)
	}

	mystery := findModelRow(t, resp.Models, "mystery-model")
	if mystery.Priced || mystery.Price.Source != priceSourceNone || mystery.Totals.CostUSD != 0 {
		t.Fatalf("unpriced model row = %+v", mystery)
	}
	if mystery.Totals.UncachedInputTokens != 1000 {
		t.Fatalf("unpriced model still counts tokens, got %d", mystery.Totals.UncachedInputTokens)
	}
	if resp.Models[0].Model != "claude-sonnet-4-5" {
		t.Fatalf("models should sort by cost, first = %q", resp.Models[0].Model)
	}
}

func TestSummaryZeroFilledHourlySeries(t *testing.T) {
	tr := newTestTracker(t.TempDir(), 0, testNow)
	tr.Record(context.Background(), attemptRecord("sk-alpha-0123456789", "claude", "m", testNow.Add(-15*time.Minute)))

	resp := tr.Summary(mustQuery(t, url.Values{"range": {"24h"}}, testNow), nil, nil)
	if resp.Bucket != bucketHour {
		t.Fatalf("bucket = %q, want hour", resp.Bucket)
	}
	if len(resp.Series) != 24 {
		t.Fatalf("series length = %d, want 24", len(resp.Series))
	}
	if resp.Series[0].T != "2026-10-17T14:00:00Z" || resp.Series[23].T != "2026-10-18T13:00:00Z" {
		t.Fatalf("series bounds = %s .. %s", resp.Series[0].T, resp.Series[23].T)
	}
	for i, point := range resp.Series {
		want := int64(0)
		if i == 23 {
			want = 1
		}
		if point.Requests != want || point.Attempts != want {
			t.Fatalf("series[%d] requests/attempts = %d/%d, want %d", i, point.Requests, point.Attempts, want)
		}
	}
}

func TestSummaryZeroFilledDailySeries(t *testing.T) {
	tr := newTestTracker(t.TempDir(), 0, testNow)
	tr.Record(context.Background(), attemptRecord("sk-alpha-0123456789", "claude", "m", time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC)))

	resp := tr.Summary(mustQuery(t, url.Values{"range": {"7d"}}, testNow), nil, nil)
	if resp.Bucket != bucketDay {
		t.Fatalf("bucket = %q, want day", resp.Bucket)
	}
	// The window starts at 14:00 on 2026-10-11, so the day containing it is the
	// first slot and the day containing the end of the window is the last one.
	if len(resp.Series) != 8 {
		t.Fatalf("series length = %d, want 8", len(resp.Series))
	}
	if resp.Series[0].T != "2026-10-11T00:00:00Z" || resp.Series[7].T != "2026-10-18T00:00:00Z" {
		t.Fatalf("series bounds = %s .. %s", resp.Series[0].T, resp.Series[7].T)
	}
	if resp.Series[4].Requests != 1 {
		t.Fatalf("series[4] (2026-10-15) requests = %d, want 1", resp.Series[4].Requests)
	}
}

func TestSummaryDayBucketsUseLocalTimezone(t *testing.T) {
	tr := newTestTracker(t.TempDir(), 0, testNow)
	ctx := context.Background()
	const key = "sk-alpha-0123456789"
	ist, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}
	// 20:00Z is 01:30 on the 18th in IST; 10:00Z and 19:00Z on the 18th are both
	// 18th-19th in IST, so the local days differ from the UTC days.
	tr.Record(ctx, attemptRecord(key, "claude", "m", time.Date(2026, 10, 17, 20, 0, 0, 0, time.UTC)))
	tr.Record(ctx, attemptRecord(key, "claude", "m", time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)))
	tr.Record(ctx, attemptRecord(key, "claude", "m", time.Date(2026, 10, 18, 19, 0, 0, 0, time.UTC)))

	// [2026-10-18 00:00 IST, 2026-10-20 00:00 IST).
	from := time.Date(2026, 10, 17, 18, 30, 0, 0, time.UTC)
	to := time.Date(2026, 10, 19, 18, 30, 0, 0, time.UTC)

	resp := tr.Summary(SummaryQuery{From: from, To: to, Bucket: bucketDay, Location: ist}, nil, nil)
	if resp.TZ != "Asia/Kolkata" {
		t.Fatalf("tz = %q, want Asia/Kolkata", resp.TZ)
	}
	if len(resp.Series) != 2 {
		t.Fatalf("IST series length = %d, want 2", len(resp.Series))
	}
	if resp.Series[0].T != "2026-10-17T18:30:00Z" || resp.Series[1].T != "2026-10-18T18:30:00Z" {
		t.Fatalf("IST series starts = %s, %s", resp.Series[0].T, resp.Series[1].T)
	}
	if resp.Series[0].Requests != 2 || resp.Series[1].Requests != 1 || resp.Totals.Requests != 3 {
		t.Fatalf("IST day requests = %d, %d, total %d; want 2, 1, 3",
			resp.Series[0].Requests, resp.Series[1].Requests, resp.Totals.Requests)
	}

	utc := tr.Summary(SummaryQuery{From: from, To: to, Bucket: bucketDay, Location: time.UTC}, nil, nil)
	if len(utc.Series) != 3 {
		t.Fatalf("UTC series length = %d, want 3", len(utc.Series))
	}
	if utc.Series[0].Requests != 1 || utc.Series[1].Requests != 2 || utc.Series[2].Requests != 0 {
		t.Fatalf("UTC day requests = %d, %d, %d; want 1, 2, 0",
			utc.Series[0].Requests, utc.Series[1].Requests, utc.Series[2].Requests)
	}
}

func TestSummaryCompactedDayBucketLandsOnLocalDayOfItsUTCStart(t *testing.T) {
	tr := newTestTracker(t.TempDir(), 0, testNow)
	ist, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}
	dayKey := bucketKey{
		start:    time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC),
		span:     bucketDay,
		keyID:    keyIDForRaw("sk-alpha-0123456789"),
		provider: "claude",
		model:    "m",
	}
	tr.buckets[dayKey] = &counters{Requests: 3, Attempts: 3}

	// 00:00Z on the 10th is 05:30 IST on the 10th.
	from := time.Date(2026, 10, 9, 18, 30, 0, 0, time.UTC)
	to := time.Date(2026, 10, 11, 18, 30, 0, 0, time.UTC)
	resp := tr.Summary(SummaryQuery{From: from, To: to, Bucket: bucketDay, Location: ist}, nil, nil)
	if len(resp.Series) != 2 || resp.Series[0].Requests != 3 || resp.Series[1].Requests != 0 {
		t.Fatalf("series = %+v", resp.Series)
	}
}

func TestSummaryJSONShapeNeverUsesNull(t *testing.T) {
	tr := newTestTracker(t.TempDir(), 0, testNow)
	resp := tr.Summary(mustQuery(t, url.Values{"range": {"24h"}}, testNow), nil, nil)
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal summary: %v", err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode summary: %v", err)
	}
	for _, field := range []string{"enabled", "from", "to", "bucket", "tz", "generated_at", "pricing", "totals", "series", "keys", "models"} {
		if _, ok := doc[field]; !ok {
			t.Fatalf("summary is missing field %q", field)
		}
	}
	if string(doc["enabled"]) != "true" {
		t.Fatalf("enabled = %s, want true", doc["enabled"])
	}
	for _, field := range []string{"keys", "models"} {
		if string(doc[field]) != "[]" {
			t.Fatalf("%s = %s, want []", field, doc[field])
		}
	}
}

func TestSummaryOnNilTrackerIsDisabled(t *testing.T) {
	var tr *Tracker
	resp := tr.Summary(mustQuery(t, url.Values{"range": {"24h"}}, testNow), nil, nil)
	if resp.Enabled {
		t.Fatal("enabled = true for a nil tracker")
	}
	if resp.Keys == nil || resp.Models == nil || len(resp.Series) != 24 {
		t.Fatalf("nil tracker summary = %+v", resp)
	}
}

func TestDisabledSummaryHasEmptyLists(t *testing.T) {
	resp := DisabledSummary(mustQuery(t, url.Values{"range": {"7d"}}, testNow))
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal disabled summary: %v", err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode disabled summary: %v", err)
	}
	if string(doc["enabled"]) != "false" || resp.Bucket != bucketDay || resp.TZ != "UTC" {
		t.Fatalf("disabled summary = %s", raw)
	}
	for _, field := range []string{"series", "keys", "models"} {
		if string(doc[field]) != "[]" {
			t.Fatalf("%s = %s, want []", field, doc[field])
		}
	}
}
