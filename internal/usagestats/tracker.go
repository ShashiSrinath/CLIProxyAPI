package usagestats

import (
	"context"
	"strings"
	"sync"
	"time"

	internallogging "github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

const (
	defaultRetentionDays = 90
	flushInterval        = 30 * time.Second
	unknownValue         = "unknown"
	httpStatusBadRequest = 400
	dedupeSeparator      = "\x00"
)

// Tracker aggregates usage records per client API key, provider and model into
// hourly buckets, compacts old hours into UTC-day buckets and persists the result
// to a JSON file. Record only updates in-memory counters, so it is cheap on the
// request path; all file I/O happens in the background flush loop and Stop.
type Tracker struct {
	dataDir string
	path    string
	now     func() time.Time

	// lifecycleMu serializes Start and Stop.
	lifecycleMu sync.Mutex
	stop        chan struct{}
	done        chan struct{}

	// flushMu serializes writes of the store file.
	flushMu sync.Mutex

	// mu guards every field below.
	mu            sync.Mutex
	retentionDays int
	buckets       map[bucketKey]*counters
	keys          map[string]*keyMeta
	recent        *recentRequestSet
	dirty         bool
}

// bucketKey identifies one aggregation bucket. start is UTC: the beginning of
// the hour for hour buckets, or of the UTC day for day buckets. span keeps hour
// and day buckets that share a start instant apart.
type bucketKey struct {
	start    time.Time
	span     string
	keyID    string
	authID   string
	provider string
	model    string
}

// counters are the additive values stored per bucket.
type counters struct {
	Requests            int64 `json:"requests"`
	Attempts            int64 `json:"attempts"`
	Failed              int64 `json:"failed"`
	InputTokens         int64 `json:"input_tokens"`
	UncachedInputTokens int64 `json:"uncached_input_tokens"`
	CacheReadTokens     int64 `json:"cache_read_tokens"`
	CacheWriteTokens    int64 `json:"cache_write_tokens"`
	OutputTokens        int64 `json:"output_tokens"`
	ReasoningTokens     int64 `json:"reasoning_tokens"`
	LatencyMsSum        int64 `json:"latency_ms_sum"`
}

func (c *counters) add(o counters) {
	c.Requests += o.Requests
	c.Attempts += o.Attempts
	c.Failed += o.Failed
	c.InputTokens += o.InputTokens
	c.UncachedInputTokens += o.UncachedInputTokens
	c.CacheReadTokens += o.CacheReadTokens
	c.CacheWriteTokens += o.CacheWriteTokens
	c.OutputTokens += o.OutputTokens
	c.ReasoningTokens += o.ReasoningTokens
	c.LatencyMsSum += o.LatencyMsSum
}

// keyMeta is the per-key metadata kept so that removed keys still display.
type keyMeta struct {
	Masked    string    `json:"masked"`
	FirstSeen time.Time `json:"first_seen"`
	LastUsed  time.Time `json:"last_used"`
}

// NewTracker returns a tracker that persists to <dataDir>/usage-stats.json. A
// retentionDays value <= 0 selects the 90 day default. Call Start to load the
// stored data and begin periodic flushes.
func NewTracker(dataDir string, retentionDays int) *Tracker {
	t := &Tracker{
		dataDir:       dataDir,
		now:           time.Now,
		retentionDays: normalizeRetention(retentionDays),
		buckets:       make(map[bucketKey]*counters),
		keys:          make(map[string]*keyMeta),
		recent:        newRecentRequestSet(recentRequestTTL, recentRequestMax),
	}
	if dataDir != "" {
		t.path = storePath(dataDir)
	}
	return t
}

// Start loads the stored usage file and starts the 30 second flush loop. It is a
// no-op when the tracker is already started.
func (t *Tracker) Start(ctx context.Context) error {
	if t == nil {
		return nil
	}
	t.lifecycleMu.Lock()
	defer t.lifecycleMu.Unlock()
	if t.stop != nil {
		return nil
	}
	if t.path == "" {
		return errDataDirNotSet
	}
	if err := t.load(); err != nil {
		return err
	}
	t.stop = make(chan struct{})
	t.done = make(chan struct{})
	go t.runFlushLoop(ctx, t.stop, t.done)
	return nil
}

// Stop ends the flush loop and performs a final flush. The context bounds how long
// Stop waits for an in-progress loop iteration to finish.
func (t *Tracker) Stop(ctx context.Context) error {
	if t == nil {
		return nil
	}
	t.lifecycleMu.Lock()
	defer t.lifecycleMu.Unlock()
	if t.stop != nil {
		close(t.stop)
		select {
		case <-t.done:
		case <-ctx.Done():
		}
		t.stop = nil
		t.done = nil
	}
	return t.flush()
}

// SetRetentionDays changes how long usage is kept. Values <= 0 select the 90 day
// default. Data beyond the new window is dropped on the next flush.
func (t *Tracker) SetRetentionDays(days int) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.retentionDays = normalizeRetention(days)
}

// Record counts one usage record, which is one upstream attempt. Requests are
// counted once per inbound request id within the recent-request window.
func (t *Tracker) Record(ctx context.Context, record coreusage.Record) {
	if t == nil {
		return
	}
	modelName := trimOr(record.Model, unknownValue)
	provider := trimOr(record.Provider, unknownValue)
	apiKey := strings.TrimSpace(record.APIKey)
	keyID := keyIDForRaw(apiKey)
	requestID := strings.TrimSpace(internallogging.GetRequestID(ctx))
	if requestID == "" {
		requestID = strings.TrimSpace(record.RequestID)
	}
	failed := record.Failed || responseFailed(ctx)
	tokens := tokenCountsFor(record)
	latencyMs := record.Latency.Milliseconds()

	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	timestamp := record.RequestedAt
	if timestamp.IsZero() {
		timestamp = now
	}
	timestamp = timestamp.UTC()

	counted := requestID == "" || t.recent.add(keyID+dedupeSeparator+requestID, now)

	key := bucketKey{
		start:    timestamp.Truncate(time.Hour),
		span:     bucketHour,
		keyID:    keyID,
		authID:   record.AuthID,
		provider: provider,
		model:    modelName,
	}
	bucket := t.buckets[key]
	if bucket == nil {
		bucket = &counters{}
		t.buckets[key] = bucket
	}
	bucket.add(tokens)
	bucket.Attempts++
	if counted {
		bucket.Requests++
	}
	if failed {
		bucket.Failed++
	}
	bucket.LatencyMsSum += latencyMs

	t.touchKeyLocked(keyID, apiKey, timestamp)
	t.dirty = true
}

// Clear drops all stored usage, including the file on disk.
func (t *Tracker) Clear() error {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	t.buckets = make(map[bucketKey]*counters)
	t.keys = make(map[string]*keyMeta)
	t.recent.reset()
	t.dirty = true
	t.mu.Unlock()
	return t.flush()
}

// touchKeyLocked creates or widens the first_seen/last_used metadata of a key.
// The caller must hold t.mu.
func (t *Tracker) touchKeyLocked(keyID, apiKey string, timestamp time.Time) {
	meta := t.keys[keyID]
	if meta == nil {
		t.keys[keyID] = &keyMeta{Masked: maskKey(apiKey), FirstSeen: timestamp, LastUsed: timestamp}
		return
	}
	if timestamp.Before(meta.FirstSeen) {
		meta.FirstSeen = timestamp
	}
	if timestamp.After(meta.LastUsed) {
		meta.LastUsed = timestamp
	}
}

func (t *Tracker) runFlushLoop(ctx context.Context, stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if err := t.flush(); err != nil {
				log.WithError(err).Warn("usagestats: failed to flush usage stats")
			}
		case <-stop:
			return
		case <-ctx.Done():
			return
		}
	}
}

// flush applies compaction and retention, then writes the store file when there
// are unsaved changes. The file is written without holding t.mu so Record is not
// blocked by I/O.
func (t *Tracker) flush() error {
	if t == nil || t.path == "" {
		return nil
	}
	t.flushMu.Lock()
	defer t.flushMu.Unlock()

	t.mu.Lock()
	now := t.now()
	if t.maintainLocked(now) {
		t.dirty = true
	}
	if !t.dirty {
		t.mu.Unlock()
		return nil
	}
	file := t.snapshotLocked(now)
	t.dirty = false
	t.mu.Unlock()

	if err := writeStore(t.path, file); err != nil {
		t.mu.Lock()
		t.dirty = true
		t.mu.Unlock()
		return err
	}
	return nil
}

// rangeSnapshot copies the buckets that start within [firstStart, to) together
// with the key metadata, so that summaries are computed without holding t.mu.
func (t *Tracker) rangeSnapshot(firstStart, to time.Time) ([]bucketRow, map[string]keyMeta) {
	if t == nil {
		return nil, nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	rows := make([]bucketRow, 0, len(t.buckets))
	for key, c := range t.buckets {
		if key.start.Before(firstStart) || !key.start.Before(to) {
			continue
		}
		rows = append(rows, bucketRow{bucketKey: key, counters: *c})
	}
	metas := make(map[string]keyMeta, len(t.keys))
	for id, meta := range t.keys {
		metas[id] = *meta
	}
	return rows, metas
}

// bucketRow is an immutable copy of one bucket taken for summary aggregation.
type bucketRow struct {
	bucketKey
	counters
}

// tokenCountsFor returns the token counters of a record, using the normalized
// breakdown for the record's provider.
func tokenCountsFor(record coreusage.Record) counters {
	detail := coreusage.EnsureTokenBreakdownForProvider(record.Detail, record.Provider, record.ExecutorType)
	return tokenCountsFrom(detail)
}

// tokenCountsFrom prefers the valid normalized breakdown. When it is unusable the
// raw detail fields are used, treating cache tokens as part of the input total.
func tokenCountsFrom(detail coreusage.Detail) counters {
	breakdown := detail.TokenBreakdown
	if breakdown.Valid() {
		return counters{
			InputTokens:         breakdown.Input.TotalTokens,
			UncachedInputTokens: breakdown.Input.UncachedTokens,
			CacheReadTokens:     breakdown.Input.CacheReadTokens,
			CacheWriteTokens:    breakdown.Input.CacheWriteTokens,
			OutputTokens:        breakdown.Output.TotalTokens,
			ReasoningTokens:     breakdown.Output.ReasoningTokens,
		}
	}
	cacheRead := detail.CacheReadTokens
	if cacheRead == 0 {
		cacheRead = detail.CachedTokens
	}
	cacheWrite := detail.CacheCreationTokens
	input := max(detail.InputTokens, cacheRead+cacheWrite)
	return counters{
		InputTokens:         input,
		UncachedInputTokens: input - cacheRead - cacheWrite,
		CacheReadTokens:     cacheRead,
		CacheWriteTokens:    cacheWrite,
		OutputTokens:        detail.OutputTokens,
		ReasoningTokens:     detail.ReasoningTokens,
	}
}

// responseFailed mirrors the redisqueue plugin: a request whose recorded HTTP
// status is an error counts as failed.
func responseFailed(ctx context.Context) bool {
	status := internallogging.GetResponseStatus(ctx)
	return status >= httpStatusBadRequest
}

func trimOr(value, fallback string) string {
	if trimmed := strings.TrimSpace(value); trimmed != "" {
		return trimmed
	}
	return fallback
}

func normalizeRetention(days int) int {
	if days <= 0 {
		return defaultRetentionDays
	}
	return days
}
