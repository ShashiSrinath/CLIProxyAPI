package usagestats

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

const conditionalFixture = `{"data":[
 {"id":"deepseek/pro","pricing":{"prompt":"0.00000066","completion":"0.000001","overrides":[{"utc_days":["saturday","sunday"],"prompt":"0.00000066"},{"utc_days":["monday","tuesday","wednesday","thursday","friday"],"utc_start":100,"utc_end":400,"prompt":"0.00000132"},{"utc_days":["monday","tuesday","wednesday","thursday","friday"],"utc_start":1000,"utc_end":0,"prompt":"0.00000066"}]}},
 {"id":"minimax/flash","pricing":{"prompt":"0.00000015","completion":"0.000001","overrides":[{"utc_start":1500,"utc_end":100,"prompt":"0.000000075"}]}},
 {"id":"qwen/flash","pricing":{"prompt":"0.00000005","completion":"0.000001","overrides":[{"min_prompt_tokens":32000,"prompt":"0.0000001"},{"min_prompt_tokens":256000,"prompt":"0.0000002"}]}},
 {"id":"anthropic/sonnet","pricing":{"prompt":"0.000003","completion":"0.000015","input_cache_read":"0.0000003","input_cache_write":"0.00000375","overrides":[{"min_prompt_tokens":200000,"prompt":"0.000006","completion":"0.0000225","input_cache_read":"0.0000006","input_cache_write":"0.0000075"}]}}
]}`

func TestConditionalCatalogAndMatching(t *testing.T) {
	entries, err := parseOpenRouterModels([]byte(conditionalFixture))
	if err != nil || len(entries) != 4 {
		t.Fatalf("entries: %v %v", entries, err)
	}
	p := NewPricing("")
	p.setCatalogLocked(entries, time.Now())
	monday := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	deep := p.Resolve("deepseek", "deepseek/pro")
	for _, tc := range []struct {
		at   time.Time
		want float64
	}{{monday.Add(time.Hour), 1.32}, {monday.Add(4 * time.Hour), .66}, {monday.Add(23 * time.Hour), .66}, {monday.Add(5 * 24 * time.Hour), .66}} {
		got := deep.forPeriod(deep.period(tc.at, 0)).Input
		if !approxEqual(got, tc.want) {
			t.Fatalf("%s = %v, want %v", tc.at, got, tc.want)
		}
	}
	mini := p.Resolve("", "minimax/flash")
	for _, tc := range []struct {
		hour int
		want float64
	}{{0, .075}, {1, .15}, {15, .075}} {
		at := monday.Add(time.Duration(tc.hour) * time.Hour)
		if got := mini.forPeriod(mini.period(at, 0)).Input; !approxEqual(got, tc.want) {
			t.Fatalf("hour %d: %v", tc.hour, got)
		}
	}
	q := p.Resolve("", "qwen/flash")
	for _, tc := range []struct {
		n    int64
		want float64
	}{{31999, .05}, {32000, .1}, {256000, .2}} {
		if got := q.forPeriod(q.period(monday, tc.n)).Input; !approxEqual(got, tc.want) {
			t.Fatalf("tokens %d: %v", tc.n, got)
		}
	}
	sonnet := p.Resolve("", "anthropic/sonnet")
	tier := sonnet.forPeriod(sonnet.period(monday, 200000))
	if tier.Input != 6 || tier.Output != 22.5 || tier.CacheRead != .6 || tier.CacheWrite != 7.5 {
		t.Fatalf("tier %+v", tier)
	}
	same := q.Conditional[0]
	same.Input = 9
	q.Conditional = append(q.Conditional, same)
	if got := q.forPeriod(q.period(monday, 32000)).Input; got != 9 {
		t.Fatal(got)
	}
	start, end := 1500, 100
	wrap := ConditionalPrice{UTCDays: []string{"monday"}, UTCStart: &start, UTCEnd: &end}
	if !wrap.matches(monday.Add(24*time.Hour), 0) || wrap.matches(monday, 0) {
		t.Fatal("wrapping weekday")
	}
	end = start
	if wrap.matches(monday.Add(15*time.Hour), 0) {
		t.Fatal("equal bounds must be empty")
	}
	if _, ok := parseConditional(map[string]json.RawMessage{"unknown_condition": json.RawMessage(`true`)}, entries[0]); ok {
		t.Fatal("unknown condition accepted")
	}
}

func TestConditionalRecordSummaryPersistenceAndOverrides(t *testing.T) {
	entries, _ := parseOpenRouterModels([]byte(conditionalFixture))
	p := NewPricing("")
	p.setCatalogLocked(entries, time.Now())
	manual := 2.0
	p.Apply(config.UsagePricingConfig{Overrides: map[string]config.ModelPriceOverride{"alias": {OpenRouterID: "qwen/flash"}, "flat": {OpenRouterID: "qwen/flash", Input: &manual}}})
	if len(p.Resolve("", "alias").Conditional) != 2 || len(p.Resolve("", "flat").Conditional) != 0 {
		t.Fatal("override conditions")
	}
	now := time.Date(2026, 10, 12, 12, 30, 0, 0, time.UTC)
	dir := t.TempDir()
	tr := newTestTracker(dir, 60, now)
	tr.SetPricing(p)
	for _, tc := range []struct {
		model  string
		at     time.Time
		tokens int64
	}{{"deepseek/pro", now.Add(-11 * time.Hour), 1000000}, {"deepseek/pro", now.Add(-2 * time.Hour), 1000000}, {"qwen/flash", now, 32000}} {
		tr.Record(context.Background(), coreusage.Record{Model: tc.model, RequestedAt: tc.at, Detail: coreusage.Detail{TokenBreakdown: coreusage.NewIndependentTokenBreakdown(tc.tokens, 0, 0, 0, 0, 0)}})
	}
	q := mustQuery(t, url.Values{}, now)
	response := tr.Summary(q, nil, p)
	if !approxEqual(response.Totals.CostUSD, 1.32+.66+.0032) {
		t.Fatalf("cost %+v", response.Totals)
	}
	tr.mu.Lock()
	tr.maintainLocked(now.Add(15 * 24 * time.Hour))
	snapshot := tr.snapshotLocked(now.Add(15 * 24 * time.Hour))
	tr.mu.Unlock()
	if len(snapshot.Buckets) != 3 {
		t.Fatalf("merged buckets %d", len(snapshot.Buckets))
	}
	for _, b := range snapshot.Buckets {
		if b.Period == "" || b.Span != bucketDay {
			t.Fatalf("bucket %+v", b)
		}
	}
	if err := writeStore(filepath.Join(dir, storeFileName), snapshot); err != nil {
		t.Fatal(err)
	}
	loaded := newTestTracker(dir, 60, now.Add(15*24*time.Hour))
	if err := loaded.load(); err != nil {
		t.Fatal(err)
	}
	if len(loaded.buckets) != 3 {
		t.Fatal("periods lost")
	}
}

func TestConditionalOldCacheRefetch(t *testing.T) {
	hit := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit <- struct{}{}; http.Error(w, "offline", 500) }))
	defer server.Close()
	oldURL := openRouterModelsURL
	openRouterModelsURL = server.URL
	defer func() { openRouterModelsURL = oldURL }()
	dir := t.TempDir()
	old := openRouterCacheFile{FetchedAt: time.Now(), Models: []CatalogEntry{{ID: "vendor/model", Input: 1, Output: 2}}}
	data, _ := json.Marshal(old)
	if err := os.WriteFile(filepath.Join(dir, pricingCacheFileName), data, 0600); err != nil {
		t.Fatal(err)
	}
	p := NewPricing(dir)
	p.Start(context.Background(), config.UsagePricingConfig{})
	defer p.Stop()
	select {
	case <-hit:
	case <-time.After(5 * time.Second):
		t.Fatal("old cache did not refetch")
	}
	if p.Resolve("", "vendor/model").Input != 1 {
		t.Fatal("fallback lost")
	}
}
