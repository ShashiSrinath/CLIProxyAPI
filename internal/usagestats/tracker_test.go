package usagestats

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	internallogging "github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

var testNow = time.Date(2026, time.October, 18, 13, 30, 0, 0, time.UTC)

// newTestTracker returns a tracker in dir whose clock is pinned to now.
func newTestTracker(dir string, retentionDays int, now time.Time) *Tracker {
	tr := NewTracker(dir, retentionDays)
	tr.now = func() time.Time { return now }
	return tr
}

// attemptRecord builds one upstream attempt with independent-protocol token
// counts: 1000 uncached input, 2000 cache read, 500 cache write, 80 output and
// 20 reasoning tokens, 250ms latency.
func attemptRecord(apiKey, provider, model string, at time.Time) coreusage.Record {
	return coreusage.Record{
		APIKey:      apiKey,
		Provider:    provider,
		Model:       model,
		RequestedAt: at,
		Latency:     250 * time.Millisecond,
		Detail: coreusage.Detail{
			TokenBreakdown: coreusage.NewIndependentTokenBreakdown(1000, 2000, 500, 80, 20, 0),
		},
	}
}

// sumCounters adds every bucket of the tracker.
func sumCounters(tr *Tracker) counters {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	var total counters
	for _, c := range tr.buckets {
		total.add(*c)
	}
	return total
}

func TestRecordCountsRequestsOncePerRequestIDButEveryAttempt(t *testing.T) {
	tr := newTestTracker(t.TempDir(), 0, testNow)
	rec := attemptRecord("sk-alpha-0123456789", "claude", "claude-sonnet-4-5", testNow)

	req1 := internallogging.WithRequestID(context.Background(), "req-1")
	tr.Record(req1, rec)
	tr.Record(req1, rec) // retry of the same inbound request
	tr.Record(internallogging.WithRequestID(context.Background(), "req-2"), rec)
	// The same request id from another key is a different request.
	otherKey := attemptRecord("sk-beta-0123456789", "claude", "claude-sonnet-4-5", testNow)
	tr.Record(req1, otherKey)

	got := sumCounters(tr)
	if got.Attempts != 4 {
		t.Fatalf("attempts = %d, want 4", got.Attempts)
	}
	if got.Requests != 3 {
		t.Fatalf("requests = %d, want 3", got.Requests)
	}
}

func TestRecordFallsBackToExecutionIDAndCountsUnidentifiedRequests(t *testing.T) {
	tr := newTestTracker(t.TempDir(), 0, testNow)
	rec := attemptRecord("sk-alpha-0123456789", "claude", "m", testNow)
	rec.RequestID = "exec-1"

	tr.Record(context.Background(), rec)
	tr.Record(context.Background(), rec) // same execution id is deduplicated

	noID := attemptRecord("sk-alpha-0123456789", "claude", "m", testNow)
	tr.Record(context.Background(), noID)
	tr.Record(context.Background(), noID)

	got := sumCounters(tr)
	if got.Attempts != 4 {
		t.Fatalf("attempts = %d, want 4", got.Attempts)
	}
	if got.Requests != 3 {
		t.Fatalf("requests = %d, want 3 (one execution id plus two unidentified)", got.Requests)
	}
}

func TestRecordCountsFailedAttempts(t *testing.T) {
	tr := newTestTracker(t.TempDir(), 0, testNow)
	ok := attemptRecord("sk-alpha-0123456789", "claude", "m", testNow)

	failed := ok
	failed.Failed = true
	tr.Record(context.Background(), ok)
	tr.Record(context.Background(), failed)

	upstreamError := internallogging.WithResponseStatusHolder(context.Background())
	internallogging.SetResponseStatus(upstreamError, http.StatusBadGateway)
	tr.Record(upstreamError, ok)

	okStatus := internallogging.WithResponseStatusHolder(context.Background())
	internallogging.SetResponseStatus(okStatus, http.StatusOK)
	tr.Record(okStatus, ok)

	got := sumCounters(tr)
	if got.Attempts != 4 {
		t.Fatalf("attempts = %d, want 4", got.Attempts)
	}
	if got.Failed != 2 {
		t.Fatalf("failed = %d, want 2 (record flag and HTTP 502 only)", got.Failed)
	}
}

func TestRecordCacheTokenAccounting(t *testing.T) {
	tr := newTestTracker(t.TempDir(), 0, testNow)
	tr.Record(context.Background(), attemptRecord("sk-alpha-0123456789", "claude", "m", testNow))

	got := sumCounters(tr)
	want := counters{
		Requests:            1,
		Attempts:            1,
		InputTokens:         3500,
		UncachedInputTokens: 1000,
		CacheReadTokens:     2000,
		CacheWriteTokens:    500,
		OutputTokens:        100,
		ReasoningTokens:     20,
		LatencyMsSum:        250,
	}
	if got != want {
		t.Fatalf("counters = %+v, want %+v", got, want)
	}
}

func TestTokenCountsFallbackUsesDetailFields(t *testing.T) {
	cases := []struct {
		name   string
		detail coreusage.Detail
		want   counters
	}{
		{
			name: "cache read field",
			detail: coreusage.Detail{
				InputTokens:     1000,
				CacheReadTokens: 200,
				OutputTokens:    50,
				ReasoningTokens: 10,
			},
			want: counters{InputTokens: 1000, UncachedInputTokens: 800, CacheReadTokens: 200, OutputTokens: 50, ReasoningTokens: 10},
		},
		{
			name: "legacy cached field",
			detail: coreusage.Detail{
				InputTokens:  1000,
				CachedTokens: 300,
				OutputTokens: 5,
			},
			want: counters{InputTokens: 1000, UncachedInputTokens: 700, CacheReadTokens: 300, OutputTokens: 5},
		},
		{
			name: "cache write field",
			detail: coreusage.Detail{
				InputTokens:         100,
				CacheCreationTokens: 40,
			},
			want: counters{InputTokens: 100, UncachedInputTokens: 60, CacheWriteTokens: 40},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A zero TokenBreakdown is invalid, so the detail fields are used.
			if got := tokenCountsFrom(tc.detail); got != tc.want {
				t.Fatalf("tokenCountsFrom() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestClearRemovesStoredUsage(t *testing.T) {
	dir := t.TempDir()
	tr := newTestTracker(dir, 0, testNow)
	tr.Record(context.Background(), attemptRecord("sk-alpha-0123456789", "claude", "m", testNow))
	if err := tr.Clear(); err != nil {
		t.Fatalf("Clear() error = %v", err)
	}
	if got := sumCounters(tr); got != (counters{}) {
		t.Fatalf("counters after Clear = %+v, want zero", got)
	}
	file, err := readStoreFile(dir)
	if err != nil {
		t.Fatalf("read store: %v", err)
	}
	if len(file.Buckets) != 0 || len(file.Keys) != 0 {
		t.Fatalf("stored buckets=%d keys=%d after Clear, want 0", len(file.Buckets), len(file.Keys))
	}
}

func TestPersistenceRoundTrip(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	const rawKey = "sk-roundtrip-0001"

	tr := newTestTracker(dir, 0, testNow)
	if err := tr.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	earlier := testNow.Add(-time.Hour)
	tr.Record(internallogging.WithRequestID(ctx, "r1"), attemptRecord(rawKey, "claude", "m1", earlier))
	tr.Record(internallogging.WithRequestID(ctx, "r2"), attemptRecord(rawKey, "claude", "m1", testNow))
	if err := tr.Stop(ctx); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}

	data, err := os.ReadFile(storePath(dir))
	if err != nil {
		t.Fatalf("read store: %v", err)
	}
	if strings.Contains(string(data), rawKey) {
		t.Fatal("raw client key was persisted")
	}
	info, err := os.Stat(storePath(dir))
	if err != nil {
		t.Fatalf("stat store: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("store permissions = %o, want 600", perm)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("data dir has %d entries, want only the store file", len(entries))
	}

	reloaded := newTestTracker(dir, 0, testNow)
	if err := reloaded.Start(ctx); err != nil {
		t.Fatalf("reloaded Start() error = %v", err)
	}
	defer func() {
		if err := reloaded.Stop(ctx); err != nil {
			t.Errorf("reloaded Stop() error = %v", err)
		}
	}()

	if got, want := sumCounters(reloaded), sumCounters(tr); got != want {
		t.Fatalf("reloaded counters = %+v, want %+v", got, want)
	}
	meta := reloaded.keys[keyIDForRaw(rawKey)]
	if meta == nil {
		t.Fatal("key metadata not reloaded")
	}
	if meta.Masked != maskKey(rawKey) {
		t.Fatalf("masked = %q, want %q", meta.Masked, maskKey(rawKey))
	}
	if !meta.FirstSeen.Equal(earlier) || !meta.LastUsed.Equal(testNow) {
		t.Fatalf("first_seen/last_used = %v/%v, want %v/%v", meta.FirstSeen, meta.LastUsed, earlier, testNow)
	}
}

func TestCorruptStoreIsMovedAsideAndStartsEmpty(t *testing.T) {
	cases := map[string]string{
		"invalid json":    "{not json",
		"unknown version": `{"version":2,"keys":{},"buckets":[]}`,
		"bad span":        `{"version":1,"keys":{},"buckets":[{"start":"2026-10-18T13:00:00Z","span":"week","key_id":"k_x"}]}`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := storePath(dir)
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatalf("write store: %v", err)
			}
			tr := newTestTracker(dir, 0, testNow)
			ctx := context.Background()
			if err := tr.Start(ctx); err != nil {
				t.Fatalf("Start() error = %v", err)
			}
			defer func() {
				if err := tr.Stop(ctx); err != nil {
					t.Errorf("Stop() error = %v", err)
				}
			}()

			if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("corrupt store still in place, stat error = %v", err)
			}
			backup := path + ".corrupt-" + strconv.FormatInt(testNow.Unix(), 10)
			data, err := os.ReadFile(backup)
			if err != nil {
				t.Fatalf("corrupt backup missing: %v", err)
			}
			if string(data) != content {
				t.Fatalf("backup content changed")
			}
			if got := sumCounters(tr); got != (counters{}) {
				t.Fatalf("counters after corrupt load = %+v, want zero", got)
			}
		})
	}
}

func TestRetentionDropsAndCompactsOldBuckets(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	tr := newTestTracker(dir, 90, testNow)

	const expiredKey = "sk-expired-key-0001"
	const keptKey = "sk-kept-key-000002"
	expiredAt := testNow.Add(-100 * 24 * time.Hour)
	compactedAt := testNow.Add(-20 * 24 * time.Hour)
	recentAt := testNow.Add(-2 * time.Hour)
	tr.Record(ctx, attemptRecord(expiredKey, "claude", "m", expiredAt))
	tr.Record(ctx, attemptRecord(keptKey, "claude", "m", compactedAt))
	tr.Record(ctx, attemptRecord(keptKey, "claude", "m", recentAt))
	if err := tr.flush(); err != nil {
		t.Fatalf("flush() error = %v", err)
	}

	keptID := keyIDForRaw(keptKey)
	dayKey := bucketKey{start: compactedAt.Truncate(24 * time.Hour), span: bucketDay, keyID: keptID, provider: "claude", model: "m"}
	hourKey := bucketKey{start: recentAt.Truncate(time.Hour), span: bucketHour, keyID: keptID, provider: "claude", model: "m"}
	if len(tr.buckets) != 2 {
		t.Fatalf("buckets = %d, want 2 (one compacted day, one recent hour)", len(tr.buckets))
	}
	if c := tr.buckets[dayKey]; c == nil || c.Requests != 1 {
		t.Fatalf("compacted day bucket missing or wrong: %+v", c)
	}
	if c := tr.buckets[hourKey]; c == nil || c.Requests != 1 {
		t.Fatalf("recent hour bucket missing or wrong: %+v", c)
	}
	if _, ok := tr.keys[keyIDForRaw(expiredKey)]; ok {
		t.Fatal("metadata of a key past retention was kept")
	}
	if _, ok := tr.keys[keptID]; !ok {
		t.Fatal("metadata of a recently used key was dropped")
	}

	file, err := readStoreFile(dir)
	if err != nil {
		t.Fatalf("read store: %v", err)
	}
	if len(file.Buckets) != 2 {
		t.Fatalf("persisted buckets = %d, want 2", len(file.Buckets))
	}

	tr.SetRetentionDays(7)
	if err := tr.flush(); err != nil {
		t.Fatalf("flush() after shrinking retention error = %v", err)
	}
	if len(tr.buckets) != 1 {
		t.Fatalf("buckets after 7 day retention = %d, want 1", len(tr.buckets))
	}
	if _, ok := tr.buckets[hourKey]; !ok {
		t.Fatal("recent hour bucket dropped by shorter retention")
	}
}

func TestConcurrentRecordSummaryAndFlush(t *testing.T) {
	tr := newTestTracker(t.TempDir(), 0, testNow)
	ctx := context.Background()
	query := SummaryQuery{From: testNow.Add(-24 * time.Hour), To: testNow.Add(time.Hour), Bucket: bucketHour}

	const writers = 8
	const perWriter = 200
	var wg sync.WaitGroup
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			key := "sk-writer-" + strconv.Itoa(w) + "-000000"
			for range perWriter {
				tr.Record(ctx, attemptRecord(key, "claude", "m", testNow))
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 20 {
			_ = tr.Summary(query, nil, nil)
			if err := tr.flush(); err != nil {
				t.Errorf("flush() error = %v", err)
			}
		}
	}()
	wg.Wait()

	if got := sumCounters(tr).Attempts; got != writers*perWriter {
		t.Fatalf("attempts = %d, want %d", got, writers*perWriter)
	}
}

func TestPluginForwardsToActiveTracker(t *testing.T) {
	tr := newTestTracker(t.TempDir(), 0, testNow)
	SetActive(tr)
	t.Cleanup(func() { SetActive(nil) })

	if got := Active(); got != tr {
		t.Fatalf("Active() = %p, want %p", got, tr)
	}
	p := &usagePlugin{}
	p.HandleUsage(context.Background(), attemptRecord("sk-alpha-0123456789", "claude", "m", testNow))
	if got := sumCounters(tr).Attempts; got != 1 {
		t.Fatalf("attempts after HandleUsage = %d, want 1", got)
	}

	SetActive(nil)
	p.HandleUsage(context.Background(), attemptRecord("sk-alpha-0123456789", "claude", "m", testNow))
	if got := sumCounters(tr).Attempts; got != 1 {
		t.Fatalf("attempts after disabling = %d, want 1", got)
	}
}

// readStoreFile decodes the store file in dir.
func readStoreFile(dir string) (storeFile, error) {
	data, err := os.ReadFile(storePath(dir))
	if err != nil {
		return storeFile{}, err
	}
	return decodeStore(data)
}
