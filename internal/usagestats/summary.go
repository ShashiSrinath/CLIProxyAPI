package usagestats

import (
	"cmp"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"
)

const (
	maxHourlySpan   = 8 * 24 * time.Hour
	maxSummarySpan  = 400 * 24 * time.Hour
	priceSourceNone = "none"
)

// summaryRanges maps the range query values to their span.
var summaryRanges = map[string]time.Duration{
	"24h": 24 * time.Hour,
	"7d":  7 * 24 * time.Hour,
	"30d": 30 * 24 * time.Hour,
	"90d": 90 * 24 * time.Hour,
}

// PriceResolver resolves the USD-per-1M-token price of a provider and model.
type PriceResolver interface {
	Resolve(provider, model string) Price
}

// SummaryQuery is the parsed window and bucket size of a summary request.
type SummaryQuery struct {
	From, To time.Time
	Bucket   string // "hour" or "day"
	Location *time.Location
}

// ConfiguredKey is a client API key from access.api-keys. Raw is the key itself
// and is only used to derive its id and mask; it never appears in a summary.
type ConfiguredKey struct {
	Raw       string
	Name      string
	Providers []string
}

// Stats holds the totals of a set of usage buckets.
type Stats struct {
	Requests            int64   `json:"requests"`
	Attempts            int64   `json:"attempts"`
	Failed              int64   `json:"failed"`
	InputTokens         int64   `json:"input_tokens"`
	UncachedInputTokens int64   `json:"uncached_input_tokens"`
	CacheReadTokens     int64   `json:"cache_read_tokens"`
	CacheWriteTokens    int64   `json:"cache_write_tokens"`
	OutputTokens        int64   `json:"output_tokens"`
	ReasoningTokens     int64   `json:"reasoning_tokens"`
	CostUSD             float64 `json:"cost_usd"`
	AvgLatencyMs        float64 `json:"avg_latency_ms"`
}

// SeriesPoint is the Stats of one time slot of a series.
type SeriesPoint struct {
	T string `json:"t"`
	Stats
}

// ModelKeyUsage is the share of one client key in a model's usage.
type ModelKeyUsage struct {
	ID       string  `json:"id"`
	Requests int64   `json:"requests"`
	CostUSD  float64 `json:"cost_usd"`
}

// ModelRow is the usage of one provider and model. Keys is only set on the
// top-level models list.
type ModelRow struct {
	Provider string          `json:"provider"`
	Model    string          `json:"model"`
	Totals   Stats           `json:"totals"`
	Priced   bool            `json:"priced"`
	Price    Price           `json:"price"`
	Keys     []ModelKeyUsage `json:"keys,omitempty"`
}

// KeyRow is the usage of one client API key.
type KeyRow struct {
	ID         string        `json:"id"`
	Name       string        `json:"name"`
	Masked     string        `json:"masked"`
	Configured bool          `json:"configured"`
	Providers  []string      `json:"providers"`
	FirstSeen  string        `json:"first_seen"`
	LastUsed   string        `json:"last_used"`
	Totals     Stats         `json:"totals"`
	Series     []SeriesPoint `json:"series"`
	Models     []ModelRow    `json:"models"`
}

// SummaryResponse is the body of GET /observability/usage/summary. Pricing is
// left nil by Summary; the handler fills it with the pricing status.
type SummaryResponse struct {
	Enabled     bool          `json:"enabled"`
	From        string        `json:"from"`
	To          string        `json:"to"`
	Bucket      string        `json:"bucket"`
	TZ          string        `json:"tz"`
	GeneratedAt string        `json:"generated_at"`
	Pricing     any           `json:"pricing"`
	Totals      Stats         `json:"totals"`
	Series      []SeriesPoint `json:"series"`
	Keys        []KeyRow      `json:"keys"`
	Models      []ModelRow    `json:"models"`
}

// ParseSummaryQuery parses the range, from, to, bucket and tz parameters. The
// default range ends at now rounded up to the next hour. Bucket defaults to hour
// for spans of 48 hours or less and to day otherwise.
func ParseSummaryQuery(values url.Values, now time.Time) (SummaryQuery, error) {
	loc := time.UTC
	if tz := strings.TrimSpace(values.Get("tz")); tz != "" {
		parsed, err := time.LoadLocation(tz)
		if err != nil {
			return SummaryQuery{}, fmt.Errorf("invalid tz %q: %w", tz, err)
		}
		loc = parsed
	}

	rangeParam := strings.TrimSpace(values.Get("range"))
	fromParam := strings.TrimSpace(values.Get("from"))
	toParam := strings.TrimSpace(values.Get("to"))

	var from, to time.Time
	if fromParam != "" || toParam != "" {
		if rangeParam != "" {
			return SummaryQuery{}, fmt.Errorf("range cannot be combined with from or to")
		}
		if fromParam == "" {
			return SummaryQuery{}, fmt.Errorf("from is required when to is set")
		}
		parsedFrom, err := time.Parse(time.RFC3339, fromParam)
		if err != nil {
			return SummaryQuery{}, fmt.Errorf("invalid from %q: %w", fromParam, err)
		}
		from = parsedFrom
		to = ceilToHour(now)
		if toParam != "" {
			parsedTo, errTo := time.Parse(time.RFC3339, toParam)
			if errTo != nil {
				return SummaryQuery{}, fmt.Errorf("invalid to %q: %w", toParam, errTo)
			}
			to = parsedTo
		}
		if !to.After(from) {
			return SummaryQuery{}, fmt.Errorf("to must be after from")
		}
	} else {
		if rangeParam == "" {
			rangeParam = "24h"
		}
		span, ok := summaryRanges[rangeParam]
		if !ok {
			return SummaryQuery{}, fmt.Errorf("invalid range %q", rangeParam)
		}
		to = ceilToHour(now)
		from = to.Add(-span)
	}

	span := to.Sub(from)
	if span > maxSummarySpan {
		return SummaryQuery{}, fmt.Errorf("time range exceeds %d days", int(maxSummarySpan.Hours()/24))
	}

	bucket := strings.ToLower(strings.TrimSpace(values.Get("bucket")))
	switch bucket {
	case "":
		bucket = bucketDay
		if span <= 48*time.Hour {
			bucket = bucketHour
		}
	case bucketHour, bucketDay:
	default:
		return SummaryQuery{}, fmt.Errorf("invalid bucket %q", bucket)
	}
	if bucket == bucketHour && span > maxHourlySpan {
		return SummaryQuery{}, fmt.Errorf("hour buckets support at most 8 days, use bucket=day")
	}

	return SummaryQuery{From: from.UTC(), To: to.UTC(), Bucket: bucket, Location: loc}, nil
}

// Summary aggregates the stored usage selected by q. Buckets are included when
// their start lies in [from truncated to the hour, to). keys are the configured
// client keys, which are listed even without usage. prices resolves model prices
// at query time, so price changes apply retroactively. A nil tracker yields an
// empty response with enabled set to false.
func (t *Tracker) Summary(q SummaryQuery, keys []ConfiguredKey, prices PriceResolver) SummaryResponse {
	loc := queryLocation(q)
	bucket := normalizeBucket(q.Bucket)
	from, to := q.From.UTC(), q.To.UTC()
	slots := summarySlots(from, to, bucket, loc)
	slotIndex := make(map[int64]int, len(slots))
	for i, slot := range slots {
		slotIndex[slot.Unix()] = i
	}

	rows, metas := t.rangeSnapshot(from.Truncate(time.Hour), to)
	builder := newSummaryBuilder(len(slots), prices)
	for _, row := range rows {
		id := modelID{provider: row.provider, model: row.model}
		price := builder.priceOf(id)
		cost := costOf(row.counters, price)
		slot, inSeries := slotIndex[slotStart(row.start, bucket, loc).Unix()]

		builder.totals.add(row.counters, cost)
		ka := builder.keyFor(row.keyID)
		ka.totals.add(row.counters, cost)
		ka.modelFor(id, price).totals.add(row.counters, cost)

		top := builder.modelFor(id, price)
		top.totals.add(row.counters, cost)
		perKey := top.keys[row.keyID]
		if perKey == nil {
			perKey = &aggregate{}
			top.keys[row.keyID] = perKey
		}
		perKey.add(row.counters, cost)

		if inSeries {
			builder.series[slot].add(row.counters, cost)
			ka.series[slot].add(row.counters, cost)
		}
	}

	configured := make(map[string]ConfiguredKey, len(keys))
	for _, key := range keys {
		raw := strings.TrimSpace(key.Raw)
		if raw == "" {
			continue
		}
		id := keyIDForRaw(raw)
		if _, dup := configured[id]; dup {
			continue
		}
		key.Raw = raw
		configured[id] = key
		builder.keyFor(id)
	}

	keyRows := make([]KeyRow, 0, len(builder.keys))
	for id, ka := range builder.keys {
		row := KeyRow{
			ID:        id,
			Providers: []string{},
			Totals:    ka.totals.stats(),
			Series:    seriesPoints(slots, ka.series),
			Models:    modelRows(ka.models, false),
		}
		cfg, isConfigured := configured[id]
		if isConfigured {
			row.Name = cfg.Name
			row.Configured = true
			row.Masked = maskKey(cfg.Raw)
			row.Providers = append(row.Providers, cfg.Providers...)
		}
		if meta, ok := metas[id]; ok {
			row.FirstSeen = formatTime(meta.FirstSeen)
			row.LastUsed = formatTime(meta.LastUsed)
			if !isConfigured {
				row.Masked = meta.Masked
			}
		}
		keyRows = append(keyRows, row)
	}
	slices.SortFunc(keyRows, func(a, b KeyRow) int {
		if c := compareCostThenRequests(a.Totals, b.Totals); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})

	generatedAt := time.Now()
	if t != nil {
		generatedAt = t.now()
	}
	return SummaryResponse{
		Enabled:     t != nil,
		From:        formatTime(from),
		To:          formatTime(to),
		Bucket:      bucket,
		TZ:          loc.String(),
		GeneratedAt: formatTime(generatedAt),
		Totals:      builder.totals.stats(),
		Series:      seriesPoints(slots, builder.series),
		Keys:        keyRows,
		Models:      modelRows(builder.models, true),
	}
}

// DisabledSummary is the response while usage tracking is disabled: enabled is
// false, the window echoes q and every list is empty.
func DisabledSummary(q SummaryQuery) SummaryResponse {
	return SummaryResponse{
		Enabled:     false,
		From:        formatTime(q.From),
		To:          formatTime(q.To),
		Bucket:      normalizeBucket(q.Bucket),
		TZ:          queryLocation(q).String(),
		GeneratedAt: formatTime(time.Now()),
		Series:      []SeriesPoint{},
		Keys:        []KeyRow{},
		Models:      []ModelRow{},
	}
}

func queryLocation(q SummaryQuery) *time.Location {
	if q.Location == nil {
		return time.UTC
	}
	return q.Location
}

func normalizeBucket(bucket string) string {
	if bucket == bucketDay {
		return bucketDay
	}
	return bucketHour
}

// summarySlots lists the start of every series slot in [from, to). Hour slots
// start at from truncated to the hour. Day slots are local days in loc, starting
// with the day that contains from.
func summarySlots(from, to time.Time, bucket string, loc *time.Location) []time.Time {
	if !to.After(from) {
		return nil
	}
	var slots []time.Time
	if bucket == bucketDay {
		y, m, d := from.In(loc).Date()
		for i := 0; ; i++ {
			slot := time.Date(y, m, d+i, 0, 0, 0, 0, loc)
			if !slot.Before(to) {
				return slots
			}
			slots = append(slots, slot)
		}
	}
	for slot := from.Truncate(time.Hour); slot.Before(to); slot = slot.Add(time.Hour) {
		slots = append(slots, slot)
	}
	return slots
}

// slotStart returns the series slot that a bucket starting at start belongs to.
// Compacted UTC-day buckets fall into the local day that contains their start.
func slotStart(start time.Time, bucket string, loc *time.Location) time.Time {
	if bucket != bucketDay {
		return start
	}
	y, m, d := start.In(loc).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, loc)
}

// costOf prices the token counters of one bucket in USD. Cache tokens use the
// effective cache prices returned by the resolver.
func costOf(c counters, price Price) float64 {
	if price.Source == priceSourceNone {
		return 0
	}
	return (float64(c.UncachedInputTokens)*price.Input +
		float64(c.CacheReadTokens)*price.CacheRead +
		float64(c.CacheWriteTokens)*price.CacheWrite +
		float64(c.OutputTokens)*price.Output) / 1e6
}

// aggregate sums bucket counters together with their cost.
type aggregate struct {
	c    counters
	cost float64
}

func (a *aggregate) add(c counters, cost float64) {
	a.c.add(c)
	a.cost += cost
}

func (a aggregate) stats() Stats {
	s := Stats{
		Requests:            a.c.Requests,
		Attempts:            a.c.Attempts,
		Failed:              a.c.Failed,
		InputTokens:         a.c.InputTokens,
		UncachedInputTokens: a.c.UncachedInputTokens,
		CacheReadTokens:     a.c.CacheReadTokens,
		CacheWriteTokens:    a.c.CacheWriteTokens,
		OutputTokens:        a.c.OutputTokens,
		ReasoningTokens:     a.c.ReasoningTokens,
		CostUSD:             a.cost,
	}
	if a.c.Attempts > 0 {
		s.AvgLatencyMs = float64(a.c.LatencyMsSum) / float64(a.c.Attempts)
	}
	return s
}

type modelID struct {
	provider string
	model    string
}

type modelAggregate struct {
	totals aggregate
	price  Price
	keys   map[string]*aggregate
}

type keyAggregate struct {
	totals aggregate
	series []aggregate
	models map[modelID]*modelAggregate
}

// summaryBuilder accumulates one summary. It is not safe for concurrent use.
type summaryBuilder struct {
	slots    int
	resolver PriceResolver
	prices   map[modelID]Price
	totals   aggregate
	series   []aggregate
	keys     map[string]*keyAggregate
	models   map[modelID]*modelAggregate
}

func newSummaryBuilder(slots int, resolver PriceResolver) *summaryBuilder {
	return &summaryBuilder{
		slots:    slots,
		resolver: resolver,
		prices:   make(map[modelID]Price),
		series:   make([]aggregate, slots),
		keys:     make(map[string]*keyAggregate),
		models:   make(map[modelID]*modelAggregate),
	}
}

// priceOf resolves a model price once per summary.
func (b *summaryBuilder) priceOf(id modelID) Price {
	if price, ok := b.prices[id]; ok {
		return price
	}
	price := Price{Source: priceSourceNone}
	if b.resolver != nil {
		price = b.resolver.Resolve(id.provider, id.model)
	}
	b.prices[id] = price
	return price
}

func (b *summaryBuilder) keyFor(keyID string) *keyAggregate {
	ka := b.keys[keyID]
	if ka == nil {
		ka = &keyAggregate{
			series: make([]aggregate, b.slots),
			models: make(map[modelID]*modelAggregate),
		}
		b.keys[keyID] = ka
	}
	return ka
}

// modelFor returns the top-level aggregate of a model.
func (b *summaryBuilder) modelFor(id modelID, price Price) *modelAggregate {
	return modelAggregateFor(b.models, id, price)
}

// modelFor returns the aggregate of a model within one key.
func (ka *keyAggregate) modelFor(id modelID, price Price) *modelAggregate {
	return modelAggregateFor(ka.models, id, price)
}

func modelAggregateFor(models map[modelID]*modelAggregate, id modelID, price Price) *modelAggregate {
	m := models[id]
	if m == nil {
		m = &modelAggregate{price: price, keys: make(map[string]*aggregate)}
		models[id] = m
	}
	return m
}

func seriesPoints(slots []time.Time, aggs []aggregate) []SeriesPoint {
	points := make([]SeriesPoint, len(slots))
	for i, slot := range slots {
		points[i] = SeriesPoint{T: formatTime(slot), Stats: aggs[i].stats()}
	}
	return points
}

// modelRows lists models sorted by cost, then requests. withKeys adds each key's
// share of the model.
func modelRows(models map[modelID]*modelAggregate, withKeys bool) []ModelRow {
	rows := make([]ModelRow, 0, len(models))
	for id, m := range models {
		row := ModelRow{
			Provider: id.provider,
			Model:    id.model,
			Totals:   m.totals.stats(),
			Priced:   m.price.Source != priceSourceNone,
			Price:    m.price,
		}
		if withKeys {
			row.Keys = make([]ModelKeyUsage, 0, len(m.keys))
			for keyID, a := range m.keys {
				row.Keys = append(row.Keys, ModelKeyUsage{ID: keyID, Requests: a.c.Requests, CostUSD: a.cost})
			}
			slices.SortFunc(row.Keys, func(a, b ModelKeyUsage) int {
				if c := cmp.Compare(b.CostUSD, a.CostUSD); c != 0 {
					return c
				}
				if c := cmp.Compare(b.Requests, a.Requests); c != 0 {
					return c
				}
				return strings.Compare(a.ID, b.ID)
			})
		}
		rows = append(rows, row)
	}
	slices.SortFunc(rows, func(a, b ModelRow) int {
		if c := compareCostThenRequests(a.Totals, b.Totals); c != 0 {
			return c
		}
		if c := strings.Compare(a.Provider, b.Provider); c != 0 {
			return c
		}
		return strings.Compare(a.Model, b.Model)
	})
	return rows
}

// compareCostThenRequests orders larger cost first, then larger request count first.
func compareCostThenRequests(a, b Stats) int {
	if c := cmp.Compare(b.CostUSD, a.CostUSD); c != 0 {
		return c
	}
	return cmp.Compare(b.Requests, a.Requests)
}

// ceilToHour returns t rounded up to the next whole hour; whole hours are unchanged.
func ceilToHour(t time.Time) time.Time {
	floor := t.Truncate(time.Hour)
	if floor.Equal(t) {
		return floor
	}
	return floor.Add(time.Hour)
}

// formatTime renders t as RFC3339 in UTC, or "" for the zero time.
func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
