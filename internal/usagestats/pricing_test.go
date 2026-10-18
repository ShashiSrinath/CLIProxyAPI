package usagestats

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

const pricingTestCatalogBody = `{"data":[
 {"id":"anthropic/claude-sonnet-4.5","name":"Anthropic: Claude Sonnet 4.5","pricing":{"prompt":"0.000003","completion":"0.000015","input_cache_read":"0.0000003","input_cache_write":"0.00000375"}},
 {"id":"google/gemini-2.5-pro","name":"Google: Gemini 2.5 Pro","pricing":{"prompt":"0.00000125","completion":"0.00001","input_cache_read":"0.000000125"}},
 {"id":"openrouter/auto","name":"Auto","pricing":{"prompt":"-1","completion":"-1"}}
]}`

// pricingFakeOpenRouter serves a configurable catalog body and counts requests.
type pricingFakeOpenRouter struct {
	srv    *httptest.Server
	hits   atomic.Int32
	mu     sync.Mutex
	body   string
	status int
}

func newPricingFakeOpenRouter(t *testing.T, body string) *pricingFakeOpenRouter {
	t.Helper()
	f := &pricingFakeOpenRouter{body: body, status: http.StatusOK}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		f.mu.Lock()
		body, status := f.body, f.status
		f.mu.Unlock()
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(f.srv.Close)
	pricingSetTestURL(t, f.srv.URL)
	return f
}

func (f *pricingFakeOpenRouter) set(body string, status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.body = body
	f.status = status
}

// pricingSetTestURL points the catalog URL at url for the rest of the test.
func pricingSetTestURL(t *testing.T, url string) {
	t.Helper()
	prev := openRouterModelsURL
	openRouterModelsURL = url
	t.Cleanup(func() { openRouterModelsURL = prev })
}

// newTestPricing returns a Pricing whose catalog was fetched from a fake server
// serving body, with cfg applied. The cache lives in a temp dir.
func newTestPricing(t *testing.T, body string, cfg config.UsagePricingConfig) (*Pricing, *pricingFakeOpenRouter) {
	t.Helper()
	fake := newPricingFakeOpenRouter(t, body)
	p := NewPricing(t.TempDir())
	p.SetHTTPClient(fake.srv.Client())
	t.Cleanup(p.Stop)
	if err := p.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	p.Apply(cfg)
	return p, fake
}

func pricingEventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func pricingPtr(v float64) *float64 { return &v }

func pricingNear(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func pricingCatalogIDs(entries []CatalogEntry) []string {
	ids := make([]string, 0, len(entries))
	for _, e := range entries {
		ids = append(ids, e.ID)
	}
	return ids
}

func pricingEntry(t *testing.T, entries []CatalogEntry, id string) CatalogEntry {
	t.Helper()
	for _, e := range entries {
		if e.ID == id {
			return e
		}
	}
	t.Fatalf("catalog has no entry %q", id)
	return CatalogEntry{}
}

func pricingExpectPrice(t *testing.T, got Price, source, matchedID string, input, output, cacheRead, cacheWrite float64) {
	t.Helper()
	if got.Source != source || got.MatchedID != matchedID {
		t.Fatalf("source/matched = %q/%q, want %q/%q (price %+v)", got.Source, got.MatchedID, source, matchedID, got)
	}
	if !pricingNear(got.Input, input) || !pricingNear(got.Output, output) ||
		!pricingNear(got.CacheRead, cacheRead) || !pricingNear(got.CacheWrite, cacheWrite) {
		t.Fatalf("prices = in %v out %v read %v write %v, want in %v out %v read %v write %v",
			got.Input, got.Output, got.CacheRead, got.CacheWrite, input, output, cacheRead, cacheWrite)
	}
}

func TestPricingParsesDecimalAndNumericPrices(t *testing.T) {
	body := `{"data":[
	 {"id":"anthropic/claude-sonnet-4.5","name":"Anthropic: Claude Sonnet 4.5","pricing":{"prompt":"0.000003","completion":"0.000015","input_cache_read":"0.0000003","input_cache_write":"0.00000375"}},
	 {"id":"numeric/model","name":"Numeric","pricing":{"prompt":0.0000005,"completion":0.0000015,"input_cache_read":0}},
	 {"id":"no/cache","name":"","pricing":{"prompt":"0.00000125","completion":"0.00001"}}
	]}`
	p, _ := newTestPricing(t, body, config.UsagePricingConfig{})

	entries := p.Catalog()
	claude := pricingEntry(t, entries, "anthropic/claude-sonnet-4.5")
	if claude.Name != "Anthropic: Claude Sonnet 4.5" || !pricingNear(claude.Input, 3) || !pricingNear(claude.Output, 15) {
		t.Fatalf("claude entry = %+v", claude)
	}
	if claude.CacheRead == nil || !pricingNear(*claude.CacheRead, 0.3) {
		t.Fatalf("claude cache_read = %v, want 0.3", claude.CacheRead)
	}
	if claude.CacheWrite == nil || !pricingNear(*claude.CacheWrite, 3.75) {
		t.Fatalf("claude cache_write = %v, want 3.75", claude.CacheWrite)
	}

	numeric := pricingEntry(t, entries, "numeric/model")
	if !pricingNear(numeric.Input, 0.5) || !pricingNear(numeric.Output, 1.5) {
		t.Fatalf("numeric entry = %+v, want input 0.5 output 1.5", numeric)
	}
	if numeric.CacheRead == nil || *numeric.CacheRead != 0 {
		t.Fatalf("numeric cache_read = %v, want 0", numeric.CacheRead)
	}

	noCache := pricingEntry(t, entries, "no/cache")
	if noCache.CacheRead != nil || noCache.CacheWrite != nil {
		t.Fatalf("omitted cache prices must be nil, got read %v write %v", noCache.CacheRead, noCache.CacheWrite)
	}
	if !pricingNear(noCache.Input, 1.25) {
		t.Fatalf("no/cache input = %v, want 1.25", noCache.Input)
	}
}

func TestPricingSkipsVariantsNegativeAndUnpricedModels(t *testing.T) {
	body := `{"data":[
	 {"id":"anthropic/claude-sonnet-4.5","name":"A","pricing":{"prompt":"0.000003","completion":"0.000015"}},
	 {"id":"x/paid:free","name":"Free","pricing":{"prompt":"0","completion":"0"}},
	 {"id":"openrouter/auto","name":"Auto","pricing":{"prompt":"-1","completion":"-1"}},
	 {"id":"neg/cache","name":"Neg","pricing":{"prompt":"0.000001","completion":"0.000001","input_cache_write":"-1"}},
	 {"id":"no/pricing","name":"None"},
	 {"id":"anthropic/claude-sonnet-4.5","name":"Dup","pricing":{"prompt":"0.000009","completion":"0.000009"}}
	]}`
	p, _ := newTestPricing(t, body, config.UsagePricingConfig{})

	ids := pricingCatalogIDs(p.Catalog())
	if len(ids) != 1 || ids[0] != "anthropic/claude-sonnet-4.5" {
		t.Fatalf("catalog ids = %v, want only anthropic/claude-sonnet-4.5", ids)
	}
	if got := pricingEntry(t, p.Catalog(), "anthropic/claude-sonnet-4.5"); !pricingNear(got.Input, 3) {
		t.Fatalf("duplicate id must keep the first entry, got input %v", got.Input)
	}
}

func TestPricingRejectsOversizedBody(t *testing.T) {
	prev := openRouterMaxBodyBytes
	openRouterMaxBodyBytes = 64
	t.Cleanup(func() { openRouterMaxBodyBytes = prev })

	newPricingFakeOpenRouter(t, pricingTestCatalogBody)
	p := NewPricing(t.TempDir())
	t.Cleanup(p.Stop)
	err := p.Refresh(context.Background())
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("Refresh error = %v, want size limit error", err)
	}
	if p.Status().CatalogSize != 0 {
		t.Fatalf("oversized body must not populate the catalog")
	}
}

func TestNormalizeModelName(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"claude-sonnet-4-5-20250929", "claude-sonnet-4-5"},
		{"anthropic/claude-sonnet-4.5", "claude-sonnet-4-5"},
		{"gemini-2.5-pro", "gemini-2-5-pro"},
		{"google/gemini-2.5-pro", "gemini-2-5-pro"},
		{"gpt-5(high)", "gpt-5"},
		{"GPT-5 (High)", "gpt-5"},
		{"openai/gpt-5:free", "gpt-5"},
		{"gpt-5-2025-08-07", "gpt-5"},
		{"  Qwen/Qwen3-Coder  ", "qwen3-coder"},
		{"deepseek-chat-v3.1", "deepseek-chat-v3-1"},
		{"claude-opus-4-1-20250805(max)", "claude-opus-4-1"},
		{"claude-sonnet-4-5", "claude-sonnet-4-5"},
	}
	for _, tc := range cases {
		if got := normalizeModelName(tc.in); got != tc.want {
			t.Errorf("normalizeModelName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestPricingVendorPreference(t *testing.T) {
	body := `{"data":[
	 {"id":"alpha/gpt-5","name":"Alpha","pricing":{"prompt":"0.000001","completion":"0.000002"}},
	 {"id":"openai/gpt-5","name":"OpenAI","pricing":{"prompt":"0.000002","completion":"0.000004"}}
	]}`
	p, _ := newTestPricing(t, body, config.UsagePricingConfig{})

	pricingExpectPrice(t, p.Resolve("codex", "gpt-5"), "openrouter", "openai/gpt-5", 2, 4, 2, 2)
	pricingExpectPrice(t, p.Resolve("openai", "gpt-5"), "openrouter", "openai/gpt-5", 2, 4, 2, 2)
	// iflow has no vendor preference, so the first catalog entry wins.
	pricingExpectPrice(t, p.Resolve("iflow", "gpt-5"), "openrouter", "alpha/gpt-5", 1, 2, 1, 1)
	// A provider whose vendor has no entry also falls back to the first entry.
	pricingExpectPrice(t, p.Resolve("claude", "gpt-5"), "openrouter", "alpha/gpt-5", 1, 2, 1, 1)
}

func TestPricingOverrideExactBeforeGlob(t *testing.T) {
	cfg := config.UsagePricingConfig{Overrides: map[string]config.ModelPriceOverride{
		"claude-opus-4-1*":        {Input: pricingPtr(1), Output: pricingPtr(2)},
		"claude-opus-4-1-special": {Input: pricingPtr(7), Output: pricingPtr(8)},
		"claude-*":                {Input: pricingPtr(5), Output: pricingPtr(5)},
		"claude-opus-*":           {Input: pricingPtr(6), Output: pricingPtr(6)},
		"claude-?":                {Input: pricingPtr(9), Output: pricingPtr(9)},
	}}
	p, _ := newTestPricing(t, pricingTestCatalogBody, cfg)

	// Exact key beats every glob that also matches it.
	pricingExpectPrice(t, p.Resolve("claude", "claude-opus-4-1-special"), "override", "claude-opus-4-1-special", 7, 8, 7, 7)
	// Longest matching glob wins.
	pricingExpectPrice(t, p.Resolve("claude", "claude-opus-4-1"), "override", "claude-opus-4-1*", 1, 2, 1, 1)
	pricingExpectPrice(t, p.Resolve("claude", "claude-opus-4-5"), "override", "claude-opus-*", 6, 6, 6, 6)
	// Equal-length patterns that both match: string order decides ("claude-*" < "claude-?").
	pricingExpectPrice(t, p.Resolve("claude", "claude-1"), "override", "claude-*", 5, 5, 5, 5)
	// Overrides are also matched against the lowercased model name.
	pricingExpectPrice(t, p.Resolve("claude", "Claude-Opus-4-5"), "override", "claude-opus-*", 6, 6, 6, 6)
}

func TestPricingOverrideMergesOpenRouterID(t *testing.T) {
	cfg := config.UsagePricingConfig{Overrides: map[string]config.ModelPriceOverride{
		"my-sonnet":       {OpenRouterID: "anthropic/claude-sonnet-4.5", Output: pricingPtr(20)},
		"my-sonnet-cache": {OpenRouterID: "anthropic/claude-sonnet-4.5", CacheRead: pricingPtr(1)},
		"my-local":        {Input: pricingPtr(0), Output: pricingPtr(0)},
		"my-gemini":       {Input: pricingPtr(2)},
		"gem-or":          {OpenRouterID: "google/gemini-2.5-pro"},
	}}
	p, _ := newTestPricing(t, pricingTestCatalogBody, cfg)

	// Manual output wins; input and cache prices come from the OpenRouter entry.
	pricingExpectPrice(t, p.Resolve("claude", "my-sonnet"), "override", "anthropic/claude-sonnet-4.5", 3, 20, 0.3, 3.75)
	// Manual cache read wins; cache write still comes from OpenRouter.
	pricingExpectPrice(t, p.Resolve("claude", "my-sonnet-cache"), "override", "anthropic/claude-sonnet-4.5", 3, 15, 1, 3.75)
	// Manual-only override with zero prices.
	pricingExpectPrice(t, p.Resolve("claude", "my-local"), "override", "my-local", 0, 0, 0, 0)
	// Unset cache prices fall back to the effective input price, not the catalog.
	pricingExpectPrice(t, p.Resolve("gemini", "my-gemini"), "override", "my-gemini", 2, 0, 2, 2)
	// OpenRouter entry without cache_write falls back to input.
	pricingExpectPrice(t, p.Resolve("gemini", "gem-or"), "override", "google/gemini-2.5-pro", 1.25, 10, 0.125, 1.25)
}

func TestPricingUnresolvedOpenRouterIDFallsThrough(t *testing.T) {
	cfg := config.UsagePricingConfig{Overrides: map[string]config.ModelPriceOverride{
		"claude-sonnet-4-5*": {OpenRouterID: "nope/nothing"},
		"manual-sonnet":      {OpenRouterID: "nope/nothing", Input: pricingPtr(4)},
	}}
	p, _ := newTestPricing(t, pricingTestCatalogBody, cfg)

	// Only an unresolved openrouter-id: fall through to the catalog match.
	pricingExpectPrice(t, p.Resolve("claude", "claude-sonnet-4-5-20250929"), "openrouter", "anthropic/claude-sonnet-4.5", 3, 15, 0.3, 3.75)
	// Manual fields make the override usable even when openrouter-id does not resolve.
	pricingExpectPrice(t, p.Resolve("claude", "manual-sonnet"), "override", "manual-sonnet", 4, 0, 4, 4)
}

func TestPricingThinkingSuffixSecondChance(t *testing.T) {
	p, _ := newTestPricing(t, pricingTestCatalogBody, config.UsagePricingConfig{})

	pricingExpectPrice(t, p.Resolve("claude", "claude-sonnet-4-5-thinking"), "openrouter", "anthropic/claude-sonnet-4.5", 3, 15, 0.3, 3.75)
	pricingExpectPrice(t, p.Resolve("claude", "claude-sonnet-4-5-20250929-thinking"), "openrouter", "anthropic/claude-sonnet-4.5", 3, 15, 0.3, 3.75)
	pricingExpectPrice(t, p.Resolve("gemini", "gemini-2.5-pro"), "openrouter", "google/gemini-2.5-pro", 1.25, 10, 0.125, 1.25)
	pricingExpectPrice(t, p.Resolve("gemini", "unknown-model"), "none", "", 0, 0, 0, 0)
}

func TestPricingReusesFreshCacheWithoutNetwork(t *testing.T) {
	fake := newPricingFakeOpenRouter(t, pricingTestCatalogBody)
	dir := t.TempDir()

	first := NewPricing(dir)
	first.SetHTTPClient(fake.srv.Client())
	t.Cleanup(first.Stop)
	if err := first.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got := fake.hits.Load(); got != 1 {
		t.Fatalf("hits after Refresh = %d, want 1", got)
	}
	if _, err := os.Stat(filepath.Join(dir, pricingCacheFileName)); err != nil {
		t.Fatalf("cache file not written: %v", err)
	}

	second := NewPricing(dir)
	second.SetHTTPClient(fake.srv.Client())
	second.Start(context.Background(), config.UsagePricingConfig{})
	status := second.Status()
	if status.Source != "cache" || status.CatalogSize != 2 || status.Error != "" || status.FetchedAt.IsZero() {
		t.Fatalf("status after cached start = %+v", status)
	}
	pricingExpectPrice(t, second.Resolve("claude", "claude-sonnet-4-5-20250929"), "openrouter", "anthropic/claude-sonnet-4.5", 3, 15, 0.3, 3.75)
	second.Stop()

	if got := fake.hits.Load(); got != 1 {
		t.Fatalf("fresh cache must not hit the network, hits = %d", got)
	}
}

func TestPricingStaleCacheIsRefreshedInBackground(t *testing.T) {
	fake := newPricingFakeOpenRouter(t, pricingTestCatalogBody)
	dir := t.TempDir()
	writePricingTestCache(t, dir, time.Now().Add(-48*time.Hour))

	p := NewPricing(dir)
	p.SetHTTPClient(fake.srv.Client())
	t.Cleanup(p.Stop)
	p.Start(context.Background(), config.UsagePricingConfig{})

	pricingEventually(t, "background refresh", func() bool {
		return p.Status().Source == "openrouter"
	})
	if got := fake.hits.Load(); got != 1 {
		t.Fatalf("hits = %d, want 1", got)
	}
	if ids := pricingCatalogIDs(p.Catalog()); len(ids) != 2 {
		t.Fatalf("catalog after refresh = %v, want the fetched entries", ids)
	}
}

func TestPricingFetchFailureKeepsCatalog(t *testing.T) {
	fake := newPricingFakeOpenRouter(t, pricingTestCatalogBody)
	p := NewPricing(t.TempDir())
	p.SetHTTPClient(fake.srv.Client())
	t.Cleanup(p.Stop)

	if err := p.Refresh(context.Background()); err != nil {
		t.Fatalf("first Refresh: %v", err)
	}
	fake.set("boom", http.StatusInternalServerError)
	if err := p.Refresh(context.Background()); err == nil {
		t.Fatalf("Refresh against a failing server must return an error")
	}

	if got := len(p.Catalog()); got != 2 {
		t.Fatalf("catalog after failed refresh has %d entries, want the old 2", got)
	}
	status := p.Status()
	if status.Error == "" || status.Source != "openrouter" || status.CatalogSize != 2 {
		t.Fatalf("status after failed refresh = %+v", status)
	}
	pricingExpectPrice(t, p.Resolve("claude", "claude-sonnet-4-5"), "openrouter", "anthropic/claude-sonnet-4.5", 3, 15, 0.3, 3.75)

	fake.set(pricingTestCatalogBody, http.StatusOK)
	if err := p.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh after recovery: %v", err)
	}
	if got := p.Status().Error; got != "" {
		t.Fatalf("error after recovery = %q, want empty", got)
	}
}

func TestPricingDisabledNeverFetches(t *testing.T) {
	fake := newPricingFakeOpenRouter(t, pricingTestCatalogBody)
	dir := t.TempDir()
	writePricingTestCache(t, dir, time.Now().Add(-48*time.Hour))

	cfg := config.UsagePricingConfig{
		OpenRouterDisabled: true,
		Overrides: map[string]config.ModelPriceOverride{
			"local-model": {Input: pricingPtr(0.5), Output: pricingPtr(1)},
		},
	}
	p := NewPricing(dir)
	p.SetHTTPClient(fake.srv.Client())
	p.Start(context.Background(), cfg)

	pricingExpectPrice(t, p.Resolve("local", "local-model"), "override", "local-model", 0.5, 1, 0.5, 0.5)
	if err := p.Refresh(context.Background()); err == nil {
		t.Fatalf("Refresh with OpenRouter disabled must return an error")
	}
	p.Stop()

	if got := fake.hits.Load(); got != 0 {
		t.Fatalf("disabled pricing hit the network %d times", got)
	}
	// The cached catalog is still usable for openrouter-id and catalog lookups.
	if status := p.Status(); status.Source != "cache" {
		t.Fatalf("status = %+v, want cache source", status)
	}
	pricingExpectPrice(t, p.Resolve("claude", "old/model"), "openrouter", "old/model", 1, 2, 1, 1)
}

func TestPricingApplyEnablingFetchesCatalog(t *testing.T) {
	fake := newPricingFakeOpenRouter(t, pricingTestCatalogBody)
	dir := t.TempDir()
	writePricingTestCache(t, dir, time.Now().Add(-48*time.Hour))

	p := NewPricing(dir)
	p.SetHTTPClient(fake.srv.Client())
	t.Cleanup(p.Stop)
	p.Start(context.Background(), config.UsagePricingConfig{OpenRouterDisabled: true})
	if got := fake.hits.Load(); got != 0 {
		t.Fatalf("disabled start hit the network %d times", got)
	}

	p.Apply(config.UsagePricingConfig{OpenRouterDisabled: false})
	pricingEventually(t, "fetch after enabling", func() bool {
		return p.Status().Source == "openrouter"
	})
}

func TestPricingStartDoesNotBlockOnFetch(t *testing.T) {
	release := make(chan struct{})
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, pricingTestCatalogBody)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	pricingSetTestURL(t, srv.URL)

	p := NewPricing(t.TempDir())
	p.SetHTTPClient(srv.Client())
	t.Cleanup(p.Stop)

	done := make(chan struct{})
	go func() {
		p.Start(context.Background(), config.UsagePricingConfig{})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Start blocked on the network fetch")
	}
	pricingEventually(t, "background fetch to start", func() bool { return hits.Load() == 1 })
}

func TestPricingStartStopIdempotent(t *testing.T) {
	fake := newPricingFakeOpenRouter(t, pricingTestCatalogBody)
	p := NewPricing(t.TempDir())
	p.SetHTTPClient(fake.srv.Client())

	p.Stop() // Stop before Start is a no-op.
	p.Start(context.Background(), config.UsagePricingConfig{})
	p.Start(context.Background(), config.UsagePricingConfig{RefreshHours: 2})
	p.Stop()
	p.Stop()

	p.Start(context.Background(), config.UsagePricingConfig{})
	p.Stop()
}

func TestPricingResolveCacheInvalidatedByApply(t *testing.T) {
	p, _ := newTestPricing(t, pricingTestCatalogBody, config.UsagePricingConfig{})

	pricingExpectPrice(t, p.Resolve("claude", "claude-sonnet-4-5"), "openrouter", "anthropic/claude-sonnet-4.5", 3, 15, 0.3, 3.75)

	p.Apply(config.UsagePricingConfig{Overrides: map[string]config.ModelPriceOverride{
		"claude-sonnet-4-5": {OpenRouterID: "anthropic/claude-sonnet-4.5", Output: pricingPtr(99)},
	}})
	pricingExpectPrice(t, p.Resolve("claude", "claude-sonnet-4-5"), "override", "anthropic/claude-sonnet-4.5", 3, 99, 0.3, 3.75)
}

func TestPricingResolveCacheInvalidatedByRefresh(t *testing.T) {
	fake := newPricingFakeOpenRouter(t, pricingTestCatalogBody)
	p := NewPricing(t.TempDir())
	p.SetHTTPClient(fake.srv.Client())
	t.Cleanup(p.Stop)
	if err := p.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	pricingExpectPrice(t, p.Resolve("claude", "claude-sonnet-4-5"), "openrouter", "anthropic/claude-sonnet-4.5", 3, 15, 0.3, 3.75)

	fake.set(`{"data":[{"id":"anthropic/claude-sonnet-4.5","name":"","pricing":{"prompt":"0.000004","completion":"0.00002"}}]}`, http.StatusOK)
	if err := p.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	pricingExpectPrice(t, p.Resolve("claude", "claude-sonnet-4-5"), "openrouter", "anthropic/claude-sonnet-4.5", 4, 20, 4, 4)
}

func TestPricingStatusAndCatalogJSON(t *testing.T) {
	empty, err := json.Marshal(PricingStatus{Source: "none"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(empty), `"fetched_at":""`) || !strings.Contains(string(empty), `"source":"none"`) {
		t.Fatalf("zero FetchedAt JSON = %s", empty)
	}

	stamped, err := json.Marshal(PricingStatus{
		Source:      "openrouter",
		FetchedAt:   time.Date(2026, 10, 18, 1, 2, 3, 0, time.UTC),
		CatalogSize: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(stamped), `"fetched_at":"2026-10-18T01:02:03Z"`) || !strings.Contains(string(stamped), `"catalog_size":2`) {
		t.Fatalf("status JSON = %s", stamped)
	}

	entry, err := json.Marshal(CatalogEntry{ID: "x/y", Input: 1, Output: 2})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(entry), `"cache_read":null`) || !strings.Contains(string(entry), `"cache_write":null`) {
		t.Fatalf("catalog entry JSON = %s", entry)
	}

	price, err := json.Marshal(Price{Source: "openrouter", MatchedID: "x/y", Input: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"source"`, `"matched_id"`, `"input"`, `"output"`, `"cache_read"`, `"cache_write"`} {
		if !strings.Contains(string(price), key) {
			t.Fatalf("price JSON %s missing %s", price, key)
		}
	}
}

func TestPricingConcurrentUse(t *testing.T) {
	fake := newPricingFakeOpenRouter(t, pricingTestCatalogBody)
	p := NewPricing(t.TempDir())
	p.SetHTTPClient(fake.srv.Client())
	t.Cleanup(p.Stop)
	if err := p.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_ = p.Resolve("claude", "claude-sonnet-4-5")
				_ = p.Status()
				_ = p.Catalog()
				if j%10 == 0 {
					p.Apply(config.UsagePricingConfig{Overrides: map[string]config.ModelPriceOverride{
						fmt.Sprintf("worker-%d*", worker): {Input: pricingPtr(float64(j))},
					}})
				}
				if worker == 0 && j == 25 {
					_ = p.Refresh(context.Background())
				}
			}
		}(i)
	}
	wg.Wait()
}

// writePricingTestCache writes a catalog cache holding one model, fetched at fetchedAt.
func writePricingTestCache(t *testing.T, dir string, fetchedAt time.Time) {
	t.Helper()
	body := fmt.Sprintf(`{"fetched_at":%q,"models":[{"id":"old/model","name":"Old","input":1,"output":2,"cache_read":null,"cache_write":null}]}`,
		fetchedAt.UTC().Format(time.RFC3339))
	if err := os.WriteFile(filepath.Join(dir, pricingCacheFileName), []byte(body), 0o600); err != nil {
		t.Fatalf("write cache: %v", err)
	}
}
