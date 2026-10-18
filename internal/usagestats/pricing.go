package usagestats

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	log "github.com/sirupsen/logrus"
)

// openRouterModelsURL is the public OpenRouter model catalog. It is a variable so
// tests can point it at a local server.
var openRouterModelsURL = "https://openrouter.ai/api/v1/models"

// openRouterMaxBodyBytes bounds how much of a catalog response is read.
var openRouterMaxBodyBytes int64 = 32 << 20

const (
	pricingCacheFileName       = "openrouter-models.json"
	pricingHTTPTimeout         = 30 * time.Second
	pricingDefaultRefreshHours = 24
	// pricingMaxRetryDelay caps the wait before retrying a failed fetch.
	pricingMaxRetryDelay = time.Hour
	// tokensPerMillion converts OpenRouter's USD-per-token prices to USD per 1M tokens.
	tokensPerMillion = 1e6
)

// errPricingDisabled is returned by Refresh when OpenRouter fetching is turned off.
var errPricingDisabled = errors.New("openrouter pricing fetch is disabled")

// Price is the effective price used for a model, in USD per one million tokens.
// Source is "override", "openrouter" or "none". MatchedID is the matched override
// or catalog id; it is empty when Source is "none".
type Price struct {
	Source     string  `json:"source"`
	MatchedID  string  `json:"matched_id"`
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
}

// CatalogEntry is one priced model from the OpenRouter catalog, in USD per one
// million tokens. CacheRead and CacheWrite are nil when OpenRouter omits them.
type CatalogEntry struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Input      float64  `json:"input"`
	Output     float64  `json:"output"`
	CacheRead  *float64 `json:"cache_read"`
	CacheWrite *float64 `json:"cache_write"`
}

// PricingStatus describes the state of the price catalog. FetchedAt marshals as
// RFC3339, or as an empty string when the catalog was never fetched.
type PricingStatus struct {
	Source      string
	FetchedAt   time.Time
	CatalogSize int
	Error       string
}

// MarshalJSON encodes the status with fetched_at as an RFC3339 string or "".
func (s PricingStatus) MarshalJSON() ([]byte, error) {
	type wire struct {
		Source      string `json:"source"`
		FetchedAt   string `json:"fetched_at"`
		CatalogSize int    `json:"catalog_size"`
		Error       string `json:"error"`
	}
	w := wire{Source: s.Source, CatalogSize: s.CatalogSize, Error: s.Error}
	if !s.FetchedAt.IsZero() {
		w.FetchedAt = s.FetchedAt.UTC().Format(time.RFC3339)
	}
	return json.Marshal(w)
}

// Pricing keeps the OpenRouter model price catalog and resolves model prices
// from it, applying configured overrides first. It is safe for concurrent use.
type Pricing struct {
	dataDir string

	// refreshMu serializes catalog fetches (scheduled and manual).
	refreshMu sync.Mutex

	mu        sync.RWMutex
	client    *http.Client
	disabled  bool
	interval  time.Duration
	overrides map[string]config.ModelPriceOverride
	// patterns holds override keys in resolution order: longest first, then by string.
	patterns []string
	// entries is the catalog in OpenRouter order.
	entries []CatalogEntry
	// byID maps a lowercased catalog id to its index in entries.
	byID map[string]int
	// byNorm maps a normalized model name to catalog indexes, in catalog order.
	byNorm map[string][]int
	// fetchedAt is the time of the last successful catalog load (cache or network).
	fetchedAt time.Time
	// live reports whether the catalog has come from a successful network fetch.
	live    bool
	lastErr string
	// resolved caches Resolve results keyed by provider, NUL and model.
	resolved map[string]Price

	started bool
	cancel  context.CancelFunc
	done    chan struct{}
	wake    chan struct{}
}

// NewPricing returns a Pricing that caches the catalog under dataDir. An empty
// dataDir disables the cache file. The catalog is empty until Start or Refresh.
func NewPricing(dataDir string) *Pricing {
	return &Pricing{
		dataDir:   dataDir,
		client:    &http.Client{Timeout: pricingHTTPTimeout},
		interval:  pricingDefaultRefreshHours * time.Hour,
		overrides: make(map[string]config.ModelPriceOverride),
		byID:      make(map[string]int),
		byNorm:    make(map[string][]int),
		resolved:  make(map[string]Price),
	}
}

// SetHTTPClient sets the client used to fetch the catalog. nil selects a default
// client with a 30 second timeout.
func (p *Pricing) SetHTTPClient(c *http.Client) {
	if c == nil {
		c = &http.Client{Timeout: pricingHTTPTimeout}
	}
	p.mu.Lock()
	p.client = c
	p.mu.Unlock()
}

// Start loads the cached catalog, fetches a new one in the background when the
// cache is missing or older than the refresh interval, and refreshes it on that
// interval. It never blocks on the network. Calling Start again applies cfg
// instead of starting another refresh loop.
func (p *Pricing) Start(ctx context.Context, cfg config.UsagePricingConfig) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.started {
		p.applyConfigLocked(cfg)
		return
	}
	p.applyLocked(cfg)
	p.loadCacheLocked()

	runCtx, cancel := context.WithCancel(ctx)
	p.cancel = cancel
	p.done = make(chan struct{})
	p.wake = make(chan struct{}, 1)
	p.started = true
	go p.run(runCtx, p.wake, p.done, p.nextFetchTimeLocked(time.Now()))
}

// Apply updates overrides, the refresh interval and the disabled flag. A changed
// interval or disabled flag restarts the refresh schedule.
func (p *Pricing) Apply(cfg config.UsagePricingConfig) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.applyConfigLocked(cfg)
}

// Stop ends the refresh loop and waits for it to exit. It is a no-op when the
// pricing is not started.
func (p *Pricing) Stop() {
	p.mu.Lock()
	if !p.started {
		p.mu.Unlock()
		return
	}
	cancel, done := p.cancel, p.done
	p.started = false
	p.cancel = nil
	p.done = nil
	p.mu.Unlock()

	cancel()
	<-done
}

// Refresh fetches the catalog now, replaces the in-memory catalog and writes the
// cache file. On failure the previous catalog is kept and Status reports the error.
// It returns an error without fetching when OpenRouter fetching is disabled.
func (p *Pricing) Refresh(ctx context.Context) error {
	p.refreshMu.Lock()
	defer p.refreshMu.Unlock()

	p.mu.RLock()
	disabled := p.disabled
	client := p.client
	p.mu.RUnlock()
	if disabled {
		return errPricingDisabled
	}

	entries, err := fetchOpenRouterCatalog(ctx, client)
	if err != nil {
		p.mu.Lock()
		p.lastErr = err.Error()
		p.mu.Unlock()
		log.Warnf("usage pricing: OpenRouter catalog refresh failed: %v", err)
		return err
	}

	fetchedAt := time.Now().UTC()
	p.mu.Lock()
	p.setCatalogLocked(entries, fetchedAt)
	p.live = true
	p.lastErr = ""
	p.mu.Unlock()

	if errCache := p.writeCache(entries, fetchedAt); errCache != nil {
		log.Warnf("usage pricing: failed to write OpenRouter cache: %v", errCache)
	}
	return nil
}

// Resolve returns the price for a model served by provider. Overrides are tried
// first (exact key, then glob patterns), then the catalog by normalized name.
// Results are cached until the catalog or overrides change.
func (p *Pricing) Resolve(provider, model string) Price {
	provider = strings.TrimSpace(provider)
	model = strings.TrimSpace(model)
	key := provider + "\x00" + model

	p.mu.RLock()
	price, ok := p.resolved[key]
	p.mu.RUnlock()
	if ok {
		return price
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if cached, found := p.resolved[key]; found {
		return cached
	}
	if p.resolved == nil {
		p.resolved = make(map[string]Price)
	}
	price = p.resolveLocked(provider, model)
	p.resolved[key] = price
	return price
}

// Status reports where the catalog came from and the last refresh error.
func (p *Pricing) Status() PricingStatus {
	p.mu.RLock()
	defer p.mu.RUnlock()
	source := "none"
	if len(p.entries) > 0 {
		source = "cache"
		if p.live {
			source = "openrouter"
		}
	}
	return PricingStatus{
		Source:      source,
		FetchedAt:   p.fetchedAt,
		CatalogSize: len(p.entries),
		Error:       p.lastErr,
	}
}

// Catalog returns a copy of the catalog sorted by id.
func (p *Pricing) Catalog() []CatalogEntry {
	p.mu.RLock()
	out := make([]CatalogEntry, len(p.entries))
	for i, e := range p.entries {
		out[i] = cloneCatalogEntry(e)
	}
	p.mu.RUnlock()
	slices.SortFunc(out, func(a, b CatalogEntry) int {
		return strings.Compare(a.ID, b.ID)
	})
	return out
}

// applyConfigLocked applies cfg and, when the pricing is running, wakes the
// refresh loop if the interval or the disabled flag changed. Caller holds p.mu.
func (p *Pricing) applyConfigLocked(cfg config.UsagePricingConfig) {
	intervalChanged, disabledChanged := p.applyLocked(cfg)
	if p.started && (intervalChanged || disabledChanged) {
		select {
		case p.wake <- struct{}{}:
		default:
		}
	}
}

// applyLocked stores cfg and invalidates resolved prices. It reports whether the
// refresh interval or the disabled flag changed. Caller holds p.mu.
func (p *Pricing) applyLocked(cfg config.UsagePricingConfig) (intervalChanged, disabledChanged bool) {
	interval := pricingRefreshInterval(cfg.RefreshHours)
	intervalChanged = interval != p.interval
	disabledChanged = cfg.OpenRouterDisabled != p.disabled

	p.interval = interval
	p.disabled = cfg.OpenRouterDisabled
	p.overrides = cloneOverrides(cfg.Overrides)
	p.patterns = overridePatterns(p.overrides)
	p.resolved = make(map[string]Price)
	return intervalChanged, disabledChanged
}

// setCatalogLocked replaces the catalog and rebuilds its indexes. Caller holds p.mu.
func (p *Pricing) setCatalogLocked(entries []CatalogEntry, fetchedAt time.Time) {
	p.entries = entries
	p.fetchedAt = fetchedAt
	p.byID = make(map[string]int, len(entries))
	p.byNorm = make(map[string][]int, len(entries))
	for i, e := range entries {
		p.byID[strings.ToLower(e.ID)] = i
		norm := normalizeModelName(e.ID)
		p.byNorm[norm] = append(p.byNorm[norm], i)
	}
	p.resolved = make(map[string]Price)
}

// loadCacheLocked loads the catalog cache file if present. Caller holds p.mu.
func (p *Pricing) loadCacheLocked() {
	if p.dataDir == "" {
		return
	}
	data, err := os.ReadFile(p.cachePath())
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Warnf("usage pricing: failed to read OpenRouter cache: %v", err)
		}
		return
	}
	var file openRouterCacheFile
	if errDecode := json.Unmarshal(data, &file); errDecode != nil {
		log.Warnf("usage pricing: ignoring corrupt OpenRouter cache: %v", errDecode)
		return
	}
	entries := normalizeCatalog(file.Models)
	if len(entries) == 0 {
		return
	}
	p.setCatalogLocked(entries, file.FetchedAt)
}

// writeCache atomically writes the catalog cache file.
func (p *Pricing) writeCache(entries []CatalogEntry, fetchedAt time.Time) error {
	if p.dataDir == "" {
		return nil
	}
	if err := os.MkdirAll(p.dataDir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(openRouterCacheFile{FetchedAt: fetchedAt, Models: entries}, "", "  ")
	if err != nil {
		return err
	}
	return writePricingFileAtomic(p.cachePath(), data)
}

func (p *Pricing) cachePath() string {
	return filepath.Join(p.dataDir, pricingCacheFileName)
}

// nextFetchTimeLocked returns when the next catalog fetch should run: now when
// there is no catalog or it is stale, otherwise the end of the current interval.
// Caller holds p.mu (read or write).
func (p *Pricing) nextFetchTimeLocked(now time.Time) time.Time {
	if p.fetchedAt.IsZero() || now.Before(p.fetchedAt) {
		return now
	}
	due := p.fetchedAt.Add(p.interval)
	if due.Before(now) {
		return now
	}
	return due
}

// run refreshes the catalog when due until ctx is cancelled. While fetching is
// disabled it only waits for a wake-up or cancellation.
func (p *Pricing) run(ctx context.Context, wake <-chan struct{}, done chan<- struct{}, nextAt time.Time) {
	defer close(done)
	for {
		p.mu.RLock()
		disabled := p.disabled
		p.mu.RUnlock()

		var due <-chan time.Time
		var timer *time.Timer
		if !disabled {
			timer = time.NewTimer(time.Until(nextAt))
			due = timer.C
		}
		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return
		case <-wake:
			if timer != nil {
				timer.Stop()
			}
			p.mu.RLock()
			nextAt = p.nextFetchTimeLocked(time.Now())
			p.mu.RUnlock()
			continue
		case <-due:
		}

		err := p.Refresh(ctx)
		p.mu.RLock()
		delay := p.interval
		p.mu.RUnlock()
		if err != nil && delay > pricingMaxRetryDelay {
			delay = pricingMaxRetryDelay
		}
		nextAt = time.Now().Add(delay)
	}
}

// resolveLocked implements the resolution order. Caller holds p.mu.
func (p *Pricing) resolveLocked(provider, model string) Price {
	if model == "" {
		return Price{Source: "none"}
	}
	if key, ov, matched := p.matchOverrideLocked(model); matched {
		if price, usable := p.overridePriceLocked(key, ov); usable {
			return price
		}
	}
	if e, ok := p.catalogMatchLocked(provider, model); ok {
		return Price{
			Source:     "openrouter",
			MatchedID:  e.ID,
			Input:      e.Input,
			Output:     e.Output,
			CacheRead:  valueOrDefault(e.CacheRead, e.Input),
			CacheWrite: valueOrDefault(e.CacheWrite, e.Input),
		}
	}
	return Price{Source: "none"}
}

// matchOverrideLocked finds the override for model: an exact key match against the
// raw and lowercased name first, then the glob patterns in resolution order.
// Caller holds p.mu.
func (p *Pricing) matchOverrideLocked(model string) (string, config.ModelPriceOverride, bool) {
	lower := strings.ToLower(model)
	for _, key := range []string{model, lower} {
		if ov, ok := p.overrides[key]; ok {
			return key, ov, true
		}
	}
	for _, pattern := range p.patterns {
		if globMatches(pattern, model) || globMatches(pattern, lower) {
			return pattern, p.overrides[pattern], true
		}
	}
	return "", config.ModelPriceOverride{}, false
}

// overridePriceLocked builds the price for an override. Manual fields win, unset
// fields come from the resolved OpenRouter id, and unset cache prices fall back to
// the input price. It reports false when the override has nothing usable (only an
// unresolved openrouter-id), so resolution falls through to the catalog. Caller
// holds p.mu.
func (p *Pricing) overridePriceLocked(key string, ov config.ModelPriceOverride) (Price, bool) {
	var base *CatalogEntry
	if idx, ok := p.catalogIndexByIDLocked(ov.OpenRouterID); ok {
		base = &p.entries[idx]
	}
	hasManual := ov.Input != nil || ov.Output != nil || ov.CacheRead != nil || ov.CacheWrite != nil
	if base == nil && !hasManual {
		return Price{}, false
	}

	price := Price{Source: "override", MatchedID: key}
	var baseInput, baseOutput float64
	var baseRead, baseWrite *float64
	if base != nil {
		price.MatchedID = base.ID
		baseInput, baseOutput = base.Input, base.Output
		baseRead, baseWrite = base.CacheRead, base.CacheWrite
	}
	price.Input = valueOrDefault(ov.Input, baseInput)
	price.Output = valueOrDefault(ov.Output, baseOutput)
	price.CacheRead = valueOrDefault(ov.CacheRead, valueOrDefault(baseRead, price.Input))
	price.CacheWrite = valueOrDefault(ov.CacheWrite, valueOrDefault(baseWrite, price.Input))
	return price, true
}

// catalogMatchLocked finds a catalog entry by normalized name. A trailing
// "-thinking" is stripped only when the first lookup fails. Caller holds p.mu.
func (p *Pricing) catalogMatchLocked(provider, model string) (CatalogEntry, bool) {
	norm := normalizeModelName(model)
	if norm == "" {
		return CatalogEntry{}, false
	}
	candidates := p.byNorm[norm]
	if len(candidates) == 0 {
		if base := normalizeModelName(strings.TrimSuffix(norm, "-thinking")); base != "" && base != norm {
			candidates = p.byNorm[base]
		}
	}
	if len(candidates) == 0 {
		return CatalogEntry{}, false
	}
	return p.entries[preferredCatalogIndex(p.entries, candidates, provider)], true
}

// catalogIndexByIDLocked looks up an OpenRouter model id, ignoring case. Caller holds p.mu.
func (p *Pricing) catalogIndexByIDLocked(id string) (int, bool) {
	id = strings.TrimSpace(id)
	if id == "" {
		return 0, false
	}
	idx, ok := p.byID[strings.ToLower(id)]
	return idx, ok
}

// fetchOpenRouterCatalog downloads and parses the OpenRouter model list.
func fetchOpenRouterCatalog(ctx context.Context, client *http.Client) ([]CatalogEntry, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, openRouterModelsURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("openrouter models: unexpected status %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, openRouterMaxBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("openrouter models: read body: %w", err)
	}
	if int64(len(body)) > openRouterMaxBodyBytes {
		return nil, fmt.Errorf("openrouter models: response exceeds %d bytes", openRouterMaxBodyBytes)
	}
	entries, err := parseOpenRouterModels(body)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, errors.New("openrouter models: no usable models in response")
	}
	return entries, nil
}

type openRouterModelsResponse struct {
	Data []openRouterModel `json:"data"`
}

type openRouterModel struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Pricing struct {
		Prompt          json.RawMessage `json:"prompt"`
		Completion      json.RawMessage `json:"completion"`
		InputCacheRead  json.RawMessage `json:"input_cache_read"`
		InputCacheWrite json.RawMessage `json:"input_cache_write"`
	} `json:"pricing"`
}

// openRouterCacheFile is the on-disk form of the catalog cache.
type openRouterCacheFile struct {
	FetchedAt time.Time      `json:"fetched_at"`
	Models    []CatalogEntry `json:"models"`
}

// parseOpenRouterModels converts the OpenRouter /models response into catalog
// entries priced in USD per one million tokens.
func parseOpenRouterModels(body []byte) ([]CatalogEntry, error) {
	var resp openRouterModelsResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("openrouter models: decode: %w", err)
	}
	entries := make([]CatalogEntry, 0, len(resp.Data))
	for _, m := range resp.Data {
		input, okInput := parseOpenRouterPrice(m.Pricing.Prompt)
		output, okOutput := parseOpenRouterPrice(m.Pricing.Completion)
		if !okInput || !okOutput {
			// Without input and output prices the model cannot be priced.
			continue
		}
		entry := CatalogEntry{
			ID:     strings.TrimSpace(m.ID),
			Name:   strings.TrimSpace(m.Name),
			Input:  input,
			Output: output,
		}
		if read, ok := parseOpenRouterPrice(m.Pricing.InputCacheRead); ok {
			entry.CacheRead = &read
		}
		if write, ok := parseOpenRouterPrice(m.Pricing.InputCacheWrite); ok {
			entry.CacheWrite = &write
		}
		entries = append(entries, entry)
	}
	return normalizeCatalog(entries), nil
}

// parseOpenRouterPrice reads a per-token USD price given as a decimal string or a
// number and returns it in USD per one million tokens, rounded to 1e-6. It reports
// false when the field is absent, empty or not a finite number.
func parseOpenRouterPrice(raw json.RawMessage) (float64, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return 0, false
	}
	var perToken float64
	if raw[0] == '"' {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return 0, false
		}
		text = strings.TrimSpace(text)
		if text == "" {
			return 0, false
		}
		parsed, err := strconv.ParseFloat(text, 64)
		if err != nil {
			return 0, false
		}
		perToken = parsed
	} else if err := json.Unmarshal(raw, &perToken); err != nil {
		return 0, false
	}
	if math.IsNaN(perToken) || math.IsInf(perToken, 0) {
		return 0, false
	}
	// Scale to per-million, then round to 1e-6 to drop float noise.
	return math.Round(perToken*tokensPerMillion*1e6) / 1e6, true
}

// normalizeCatalog drops entries that cannot be used: empty ids, ids containing
// ':' (variants such as ":free"), names that normalize to nothing, negative prices
// (OpenRouter's "-1" for variable routes) and duplicate ids. Order is kept.
func normalizeCatalog(entries []CatalogEntry) []CatalogEntry {
	seen := make(map[string]struct{}, len(entries))
	out := make([]CatalogEntry, 0, len(entries))
	for _, e := range entries {
		if e.ID == "" || strings.Contains(e.ID, ":") || normalizeModelName(e.ID) == "" {
			continue
		}
		if e.Input < 0 || e.Output < 0 ||
			(e.CacheRead != nil && *e.CacheRead < 0) ||
			(e.CacheWrite != nil && *e.CacheWrite < 0) {
			continue
		}
		key := strings.ToLower(e.ID)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, e)
	}
	return out
}

var (
	pricingDateSuffix  = regexp.MustCompile(`-(\d{8}|\d{4}-\d{2}-\d{2})$`)
	pricingParenSuffix = regexp.MustCompile(`\s*\([^()]*\)$`)
)

// normalizeModelName maps catalog ids and incoming model names to a shared key:
// lowercase, drop the vendor prefix, drop ":variant", drop a trailing "(...)",
// drop a trailing -YYYYMMDD or -YYYY-MM-DD date, and replace '.' with '-'.
func normalizeModelName(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s = s[i+1:]
	}
	if i := strings.Index(s, ":"); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	s = pricingParenSuffix.ReplaceAllString(s, "")
	s = pricingDateSuffix.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, ".", "-")
	return strings.TrimSpace(s)
}

// openRouterVendors maps a provider to the OpenRouter vendor prefix preferred for
// it. Providers not listed (such as iflow) have no preference.
var openRouterVendors = map[string]string{
	"claude":      "anthropic",
	"codex":       "openai",
	"openai":      "openai",
	"gemini":      "google",
	"gemini-cli":  "google",
	"vertex":      "google",
	"aistudio":    "google",
	"antigravity": "google",
	"xai":         "x-ai",
	"kimi":        "moonshotai",
	"qwen":        "qwen",
	"deepseek":    "deepseek",
}

// preferredCatalogIndex picks, among candidate catalog indexes sharing a normalized
// name, the entry from the provider's vendor, or the first in catalog order.
func preferredCatalogIndex(entries []CatalogEntry, candidates []int, provider string) int {
	if vendor := openRouterVendors[strings.ToLower(strings.TrimSpace(provider))]; vendor != "" {
		for _, idx := range candidates {
			if catalogVendor(entries[idx].ID) == vendor {
				return idx
			}
		}
	}
	return candidates[0]
}

// catalogVendor returns the lowercased vendor prefix of an id, or "" when it has none.
func catalogVendor(id string) string {
	vendor, _, found := strings.Cut(id, "/")
	if !found {
		return ""
	}
	return strings.ToLower(vendor)
}

// pricingRefreshInterval converts configured hours to a duration, defaulting to 24.
func pricingRefreshInterval(hours int) time.Duration {
	if hours <= 0 {
		hours = pricingDefaultRefreshHours
	}
	return time.Duration(hours) * time.Hour
}

// overridePatterns returns the override keys in resolution order: longest first,
// then by string. Keys that are not valid glob patterns are still usable as exact
// keys, so they are only logged here.
func overridePatterns(overrides map[string]config.ModelPriceOverride) []string {
	patterns := make([]string, 0, len(overrides))
	for pattern := range overrides {
		if _, err := path.Match(pattern, ""); err != nil {
			log.Warnf("usage pricing: override %q is not a valid glob pattern, only exact matches apply: %v", pattern, err)
		}
		patterns = append(patterns, pattern)
	}
	slices.SortFunc(patterns, func(a, b string) int {
		if len(a) != len(b) {
			return len(b) - len(a)
		}
		return strings.Compare(a, b)
	})
	return patterns
}

// globMatches reports whether name matches pattern under path.Match rules.
func globMatches(pattern, name string) bool {
	ok, err := path.Match(pattern, name)
	return err == nil && ok
}

func cloneOverrides(in map[string]config.ModelPriceOverride) map[string]config.ModelPriceOverride {
	out := make(map[string]config.ModelPriceOverride, len(in))
	for key, ov := range in {
		out[key] = config.ModelPriceOverride{
			OpenRouterID: ov.OpenRouterID,
			Input:        cloneFloat(ov.Input),
			Output:       cloneFloat(ov.Output),
			CacheRead:    cloneFloat(ov.CacheRead),
			CacheWrite:   cloneFloat(ov.CacheWrite),
		}
	}
	return out
}

func cloneCatalogEntry(e CatalogEntry) CatalogEntry {
	e.CacheRead = cloneFloat(e.CacheRead)
	e.CacheWrite = cloneFloat(e.CacheWrite)
	return e
}

func cloneFloat(v *float64) *float64 {
	if v == nil {
		return nil
	}
	c := *v
	return &c
}

// valueOrDefault returns *v when set, otherwise def.
func valueOrDefault(v *float64, def float64) float64 {
	if v != nil {
		return *v
	}
	return def
}

// writePricingFileAtomic writes data to a temporary file in the same directory and
// renames it over path, so readers never see a partial file.
func writePricingFileAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	committed := false
	defer func() {
		if !committed {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()
	if _, errWrite := f.Write(data); errWrite != nil {
		return errWrite
	}
	if errSync := f.Sync(); errSync != nil {
		return errSync
	}
	if errClose := f.Close(); errClose != nil {
		return errClose
	}
	if errRename := os.Rename(tmp, path); errRename != nil {
		return errRename
	}
	committed = true
	return nil
}
