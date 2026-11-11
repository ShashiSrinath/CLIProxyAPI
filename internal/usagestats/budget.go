package usagestats

import (
	"fmt"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	log "github.com/sirupsen/logrus"
)

const (
	// budgetCacheTTL bounds how long a computed budget status is reused on the
	// request path.
	budgetCacheTTL = 5 * time.Second

	// BudgetPeriodDay, BudgetPeriodWeek and BudgetPeriodMonth are the window
	// periods of a spend limit. Weeks start on Monday.
	BudgetPeriodDay   = "day"
	BudgetPeriodWeek  = "week"
	BudgetPeriodMonth = "month"
)

// KeyIDForRaw returns the stable, non-reversible identifier of a client API key.
func KeyIDForRaw(raw string) string {
	return keyIDForRaw(raw)
}

// MaskKeyForRaw returns the display form of a client API key. It never reveals
// enough characters to reuse the key.
func MaskKeyForRaw(raw string) string {
	return maskKey(raw)
}

// BudgetWindow is the spend of one client API key inside one calendar window.
type BudgetWindow struct {
	Period   string    `json:"period"`
	LimitUSD float64   `json:"limit_usd"`
	SpentUSD float64   `json:"spent_usd"`
	Start    time.Time `json:"start"`
	ResetsAt time.Time `json:"resets_at"`
	Exceeded bool      `json:"exceeded"`
}

// BudgetStatus is the spend status of one client API key across its configured
// windows. Only windows with a positive limit are listed.
type BudgetStatus struct {
	KeyID    string         `json:"id"`
	Windows  []BudgetWindow `json:"windows"`
	Exceeded bool           `json:"exceeded"`
}

// BudgetInterval renders a window period as an OpenRouter-style reset interval:
// "daily", "weekly" or "monthly". Unknown periods yield an empty string.
func BudgetInterval(period string) string {
	switch period {
	case BudgetPeriodDay:
		return "daily"
	case BudgetPeriodWeek:
		return "weekly"
	case BudgetPeriodMonth:
		return "monthly"
	default:
		return ""
	}
}

// BudgetWindowBounds returns the inclusive start and the exclusive reset instant
// of the spend window period that contains now in timezone. An empty or invalid
// timezone falls back to UTC.
func BudgetWindowBounds(now time.Time, timezone, period string) (time.Time, time.Time) {
	return budgetWindowBounds(now, budgetLocation(timezone), period)
}

// BudgetWindows lists the spend windows of limit that have a positive cap. Each
// window carries its bounds around now; its spend is left at zero.
func BudgetWindows(limit config.APIKeyLimit, now time.Time) []BudgetWindow {
	windows := budgetWindows(limit)
	if len(windows) == 0 {
		return windows
	}
	if now.IsZero() {
		now = time.Now()
	}
	loc := budgetLocation(limit.Timezone)
	for i := range windows {
		windows[i].Start, windows[i].ResetsAt = budgetWindowBounds(now, loc, windows[i].Period)
	}
	return windows
}

// budgetCacheEntry is one cached status together with the clock reading it was
// computed at.
type budgetCacheEntry struct {
	status BudgetStatus
	at     time.Time
}

// KeySpendSince sums the API-equivalent cost of every bucket of keyID that starts
// at or after since. Hourly buckets are exact; compacted daily buckets approximate
// the local window boundary because their start is the UTC day.
func (t *Tracker) KeySpendSince(keyID string, since time.Time) float64 {
	if t == nil || strings.TrimSpace(keyID) == "" {
		return 0
	}
	t.mu.Lock()
	rows := make([]bucketRow, 0, len(t.buckets))
	for key, c := range t.buckets {
		if key.keyID != keyID || key.start.Before(since) {
			continue
		}
		rows = append(rows, bucketRow{bucketKey: key, counters: *c})
	}
	t.mu.Unlock()
	if len(rows) == 0 {
		return 0
	}
	prices := make(map[modelID]Price)
	var resolver PriceResolver = t.priceResolver()
	var total float64
	for _, row := range rows {
		id := modelID{provider: row.provider, model: row.model}
		price, cached := prices[id]
		if !cached {
			price = Price{Source: priceSourceNone}
			if resolver != nil {
				price = resolver.Resolve(id.provider, id.model)
			}
			prices[id] = price
		}
		total += costOf(row.counters, price.forPeriod(row.period))
	}
	return total
}

// priceResolver returns the resolver currently attached to the tracker, if any.
func (t *Tracker) priceResolver() PriceResolver {
	if t == nil {
		return nil
	}
	if holder, ok := t.pricing.Load().(pricingHolder); ok {
		return holder.resolver
	}
	return nil
}

// KeyBudget reports how much of each configured window rawKey has spent at now.
// Results are cached per key and limit for budgetCacheTTL using the tracker clock.
func (t *Tracker) KeyBudget(rawKey string, limit config.APIKeyLimit, now time.Time) BudgetStatus {
	keyID := KeyIDForRaw(rawKey)
	status := BudgetStatus{KeyID: keyID, Windows: []BudgetWindow{}}
	if t == nil || !limit.HasCap() {
		return status
	}
	if now.IsZero() {
		now = t.now()
	}
	if cached, ok := t.cachedBudget(keyID, limit, now); ok {
		return cached
	}
	for _, window := range BudgetWindows(limit, now) {
		window.SpentUSD = t.KeySpendSince(keyID, window.Start)
		window.Exceeded = window.SpentUSD >= window.LimitUSD
		status.Windows = append(status.Windows, window)
		status.Exceeded = status.Exceeded || window.Exceeded
	}
	t.storeBudget(keyID, limit, now, status)
	return status
}

// budgetLocation resolves the limit timezone, falling back to UTC.
func budgetLocation(timezone string) *time.Location {
	timezone = strings.TrimSpace(timezone)
	if timezone == "" {
		return time.UTC
	}
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		log.WithError(err).WithField("timezone", timezone).Warn("usagestats: invalid api key limit timezone; using UTC")
		return time.UTC
	}
	return loc
}

// budgetWindows lists the configured windows with a positive limit.
func budgetWindows(limit config.APIKeyLimit) []BudgetWindow {
	windows := make([]BudgetWindow, 0, 3)
	if limit.DailyUSD > 0 {
		windows = append(windows, BudgetWindow{Period: BudgetPeriodDay, LimitUSD: limit.DailyUSD})
	}
	if limit.WeeklyUSD > 0 {
		windows = append(windows, BudgetWindow{Period: BudgetPeriodWeek, LimitUSD: limit.WeeklyUSD})
	}
	if limit.MonthlyUSD > 0 {
		windows = append(windows, BudgetWindow{Period: BudgetPeriodMonth, LimitUSD: limit.MonthlyUSD})
	}
	return windows
}

// budgetWindowBounds returns the inclusive start and the exclusive reset instant
// of the window containing now: local midnight today, local midnight of the most
// recent Monday and local midnight of the first day of the month.
func budgetWindowBounds(now time.Time, loc *time.Location, period string) (time.Time, time.Time) {
	local := now.In(loc)
	dayStart := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	switch period {
	case BudgetPeriodWeek:
		offset := (int(local.Weekday()) + 6) % 7
		start := dayStart.AddDate(0, 0, -offset)
		return start, start.AddDate(0, 0, 7)
	case BudgetPeriodMonth:
		start := time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, loc)
		return start, start.AddDate(0, 1, 0)
	default:
		return dayStart, dayStart.AddDate(0, 0, 1)
	}
}

// budgetCacheKey identifies a cached status. The limit values and timezone are
// part of the key so a config change is never masked by the cache.
func budgetCacheKey(keyID string, limit config.APIKeyLimit) string {
	return fmt.Sprintf("%s|%g|%g|%g|%s", keyID, limit.DailyUSD, limit.WeeklyUSD, limit.MonthlyUSD, strings.TrimSpace(limit.Timezone))
}

func (t *Tracker) cachedBudget(keyID string, limit config.APIKeyLimit, now time.Time) (BudgetStatus, bool) {
	key := budgetCacheKey(keyID, limit)
	t.budgetMu.Lock()
	defer t.budgetMu.Unlock()
	entry, ok := t.budgetCache[key]
	if !ok {
		return BudgetStatus{}, false
	}
	age := now.Sub(entry.at)
	if age < 0 || age >= budgetCacheTTL {
		delete(t.budgetCache, key)
		return BudgetStatus{}, false
	}
	return copyBudgetStatus(entry.status), true
}

func (t *Tracker) storeBudget(keyID string, limit config.APIKeyLimit, now time.Time, status BudgetStatus) {
	key := budgetCacheKey(keyID, limit)
	t.budgetMu.Lock()
	defer t.budgetMu.Unlock()
	if t.budgetCache == nil {
		t.budgetCache = make(map[string]budgetCacheEntry)
	}
	// Drop expired entries so a tracker with many keys does not grow forever.
	for cachedKey, entry := range t.budgetCache {
		age := now.Sub(entry.at)
		if age < 0 || age >= budgetCacheTTL {
			delete(t.budgetCache, cachedKey)
		}
	}
	t.budgetCache[key] = budgetCacheEntry{status: copyBudgetStatus(status), at: now}
}

func copyBudgetStatus(status BudgetStatus) BudgetStatus {
	out := status
	out.Windows = append([]BudgetWindow(nil), status.Windows...)
	return out
}
