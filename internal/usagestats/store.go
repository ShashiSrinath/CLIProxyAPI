package usagestats

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	bucketHour = "hour"
	bucketDay  = "day"

	storeFileName = "usage-stats.json"
	storeVersion  = 2

	// hourlyWindow is how long hour buckets are kept before they are compacted
	// into UTC-day buckets.
	hourlyWindow = 14 * 24 * time.Hour
)

var errDataDirNotSet = errors.New("usagestats: data dir is not set")

// storeFile is the on-disk layout of usage-stats.json.
type storeFile struct {
	Version   int                `json:"version"`
	UpdatedAt time.Time          `json:"updated_at"`
	Keys      map[string]keyMeta `json:"keys"`
	Buckets   []bucketRecord     `json:"buckets"`
}

// bucketRecord is one persisted bucket. The counters are embedded so they are
// flattened next to the bucket identity.
type bucketRecord struct {
	Start    time.Time `json:"start"`
	Span     string    `json:"span"`
	KeyID    string    `json:"key_id"`
	AuthID   string    `json:"auth_id,omitempty"`
	Provider string    `json:"provider"`
	Model    string    `json:"model"`
	counters
}

func storePath(dataDir string) string {
	return filepath.Join(dataDir, storeFileName)
}

// load reads the store file into memory. A file that cannot be decoded is moved
// aside to usage-stats.json.corrupt-<unix ts> and the tracker starts empty.
// The caller must hold t.lifecycleMu, not t.mu.
func (t *Tracker) load() error {
	data, err := os.ReadFile(t.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("usagestats: read %s: %w", t.path, err)
	}

	file, err := decodeStore(data)
	if err != nil {
		backup := fmt.Sprintf("%s.corrupt-%d", t.path, t.now().Unix())
		if errRename := os.Rename(t.path, backup); errRename != nil {
			return fmt.Errorf("usagestats: move corrupt store aside: %w", errRename)
		}
		log.WithError(err).WithField("backup", backup).Warn("usagestats: stored usage file is corrupt, starting empty")
		return nil
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	for _, rec := range file.Buckets {
		key := bucketKey{
			start:    rec.Start.UTC(),
			span:     rec.Span,
			keyID:    rec.KeyID,
			authID:   rec.AuthID,
			provider: rec.Provider,
			model:    rec.Model,
		}
		if existing := t.buckets[key]; existing != nil {
			existing.add(rec.counters)
			continue
		}
		counted := rec.counters
		t.buckets[key] = &counted
	}
	for id, meta := range file.Keys {
		if existing := t.keys[id]; existing != nil {
			if meta.FirstSeen.Before(existing.FirstSeen) {
				existing.FirstSeen = meta.FirstSeen
			}
			if meta.LastUsed.After(existing.LastUsed) {
				existing.LastUsed = meta.LastUsed
			}
			continue
		}
		loaded := meta
		t.keys[id] = &loaded
	}
	if t.maintainLocked(t.now()) {
		t.dirty = true
	}
	return nil
}

// decodeStore parses and validates a store file.
func decodeStore(data []byte) (storeFile, error) {
	var file storeFile
	if err := json.Unmarshal(data, &file); err != nil {
		return storeFile{}, err
	}
	if file.Version != 1 && file.Version != storeVersion {
		return storeFile{}, fmt.Errorf("unsupported store version %d", file.Version)
	}
	for _, rec := range file.Buckets {
		if rec.Span != bucketHour && rec.Span != bucketDay {
			return storeFile{}, fmt.Errorf("invalid bucket span %q", rec.Span)
		}
		if rec.KeyID == "" {
			return storeFile{}, fmt.Errorf("bucket without key id")
		}
	}
	return file, nil
}

// snapshotLocked builds the serializable state with buckets in a stable order.
// The caller must hold t.mu.
func (t *Tracker) snapshotLocked(now time.Time) storeFile {
	file := storeFile{
		Version:   storeVersion,
		UpdatedAt: now.UTC(),
		Keys:      make(map[string]keyMeta, len(t.keys)),
		Buckets:   make([]bucketRecord, 0, len(t.buckets)),
	}
	for id, meta := range t.keys {
		file.Keys[id] = *meta
	}
	for key, c := range t.buckets {
		file.Buckets = append(file.Buckets, bucketRecord{
			Start:    key.start,
			Span:     key.span,
			KeyID:    key.keyID,
			AuthID:   key.authID,
			Provider: key.provider,
			Model:    key.model,
			counters: *c,
		})
	}
	sortBucketRecords(file.Buckets)
	return file
}

func sortBucketRecords(records []bucketRecord) {
	slices.SortFunc(records, func(a, b bucketRecord) int {
		if c := a.Start.Compare(b.Start); c != 0 {
			return c
		}
		if c := strings.Compare(a.Span, b.Span); c != 0 {
			return c
		}
		if c := strings.Compare(a.KeyID, b.KeyID); c != 0 {
			return c
		}
		if c := strings.Compare(a.AuthID, b.AuthID); c != 0 {
			return c
		}
		if c := strings.Compare(a.Provider, b.Provider); c != 0 {
			return c
		}
		return strings.Compare(a.Model, b.Model)
	})
}

// maintainLocked drops buckets and key metadata older than the retention window
// and compacts hour buckets older than hourlyWindow into UTC-day buckets. It
// reports whether anything changed. The caller must hold t.mu.
func (t *Tracker) maintainLocked(now time.Time) bool {
	retentionCutoff := now.Add(-time.Duration(t.retentionDays) * 24 * time.Hour)
	compactCutoff := now.Add(-hourlyWindow)
	changed := false

	for key, c := range t.buckets {
		switch {
		case key.start.Before(retentionCutoff):
			delete(t.buckets, key)
			changed = true
		case key.span == bucketHour && key.start.Before(compactCutoff):
			dayKey := key
			dayKey.start = key.start.UTC().Truncate(24 * time.Hour)
			dayKey.span = bucketDay
			day := t.buckets[dayKey]
			if day == nil {
				day = &counters{}
				t.buckets[dayKey] = day
			}
			day.add(*c)
			delete(t.buckets, key)
			changed = true
		}
	}

	for id, meta := range t.keys {
		if !meta.LastUsed.IsZero() && meta.LastUsed.Before(retentionCutoff) {
			delete(t.keys, id)
			changed = true
		}
	}
	return changed
}

// writeStore serializes file and replaces path atomically.
func writeStore(path string, file storeFile) error {
	data, err := json.Marshal(file)
	if err != nil {
		return fmt.Errorf("usagestats: encode store: %w", err)
	}
	return writeFileAtomic(path, data)
}

// writeFileAtomic writes data to a temporary file in the same directory and
// renames it over path, so readers never observe a partial file.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("usagestats: create data dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("usagestats: create temp file: %w", err)
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			if errRemove := os.Remove(tmpName); errRemove != nil && !errors.Is(errRemove, fs.ErrNotExist) {
				log.WithError(errRemove).Warn("usagestats: failed to remove temp file")
			}
		}
	}()

	if _, errWrite := tmp.Write(data); errWrite != nil {
		_ = tmp.Close()
		return fmt.Errorf("usagestats: write temp file: %w", errWrite)
	}
	if errChmod := tmp.Chmod(0o600); errChmod != nil {
		_ = tmp.Close()
		return fmt.Errorf("usagestats: chmod temp file: %w", errChmod)
	}
	if errSync := tmp.Sync(); errSync != nil {
		_ = tmp.Close()
		return fmt.Errorf("usagestats: sync temp file: %w", errSync)
	}
	if errClose := tmp.Close(); errClose != nil {
		return fmt.Errorf("usagestats: close temp file: %w", errClose)
	}
	if errReplace := os.Rename(tmpName, path); errReplace != nil {
		return fmt.Errorf("usagestats: replace store file: %w", errReplace)
	}
	committed = true
	return nil
}
