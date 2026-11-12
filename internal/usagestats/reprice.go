package usagestats

import (
	"slices"
	"strings"
	"time"
)

// PricePoint is the price of a catalog model from From onward, in USD per one
// million tokens.
type PricePoint struct {
	From       time.Time `json:"from"`
	Input      float64   `json:"input"`
	Output     float64   `json:"output"`
	CacheRead  float64   `json:"cache_read"`
	CacheWrite float64   `json:"cache_write"`
}

// RepriceRequest freezes the cost of buckets that were never priced. Timeline
// maps a catalog id (the matched_id the current resolver reports) to its price
// history. A bucket takes the last point whose From is not after the bucket
// start, or the first point when it predates all of them. FreezeRest freezes
// buckets without a timeline at their current price. DryRun computes the
// report without changing anything.
type RepriceRequest struct {
	Timeline   map[string][]PricePoint `json:"timeline"`
	FreezeRest bool                    `json:"freeze_rest"`
	DryRun     bool                    `json:"dry_run"`
}

// RepriceModel reports the effect of a reprice for one provider and model.
type RepriceModel struct {
	Provider  string  `json:"provider"`
	Model     string  `json:"model"`
	MatchedID string  `json:"matched_id"`
	Source    string  `json:"source"` // "timeline", "current" or "skipped"
	Buckets   int     `json:"buckets"`
	BeforeUSD float64 `json:"before_usd"`
	AfterUSD  float64 `json:"after_usd"`
}

// RepriceReport summarizes a reprice.
type RepriceReport struct {
	DryRun    bool           `json:"dry_run"`
	Frozen    int            `json:"frozen_buckets"`
	BeforeUSD float64        `json:"before_usd"`
	AfterUSD  float64        `json:"after_usd"`
	Models    []RepriceModel `json:"models"`
}

// RepriceUnpriced freezes the cost of every unpriced bucket using req and
// writes the store when anything changed. Buckets that already carry a
// snapshotted cost are never touched, so running it twice is harmless.
func (t *Tracker) RepriceUnpriced(req RepriceRequest, resolver PriceResolver) (RepriceReport, error) {
	if t == nil {
		return RepriceReport{DryRun: req.DryRun, Models: []RepriceModel{}}, nil
	}
	report := t.repriceUnpriced(req, resolver)
	if report.Frozen == 0 {
		return report, nil
	}
	return report, t.flush()
}

func (t *Tracker) repriceUnpriced(req RepriceRequest, resolver PriceResolver) RepriceReport {
	report := RepriceReport{DryRun: req.DryRun, Models: []RepriceModel{}}
	timeline := make(map[string][]PricePoint, len(req.Timeline))
	for id, points := range req.Timeline {
		sorted := slices.Clone(points)
		slices.SortFunc(sorted, func(a, b PricePoint) int { return a.From.Compare(b.From) })
		if len(sorted) > 0 {
			timeline[strings.ToLower(strings.TrimSpace(id))] = sorted
		}
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	prices := make(map[modelID]Price)
	models := make(map[modelID]*RepriceModel)
	for key, c := range t.buckets {
		if key.priced {
			continue
		}
		id := modelID{provider: key.provider, model: key.model}
		price, ok := prices[id]
		if !ok {
			price = Price{Source: priceSourceNone}
			if resolver != nil {
				price = resolver.Resolve(id.provider, id.model)
			}
			prices[id] = price
		}
		m := models[id]
		if m == nil {
			m = &RepriceModel{Provider: id.provider, Model: id.model, MatchedID: price.MatchedID, Source: "skipped"}
			models[id] = m
		}
		before := costOf(*c, price.forPeriod(key.period))
		m.Buckets++
		m.BeforeUSD += before
		report.BeforeUSD += before

		after := before
		freeze := false
		if points := timeline[strings.ToLower(price.MatchedID)]; price.MatchedID != "" && len(points) > 0 {
			p := pricePointAt(points, key.start)
			after = costOf(*c, Price{Source: "timeline", Input: p.Input, Output: p.Output, CacheRead: p.CacheRead, CacheWrite: p.CacheWrite})
			m.Source = "timeline"
			freeze = true
		} else if req.FreezeRest && price.Source != priceSourceNone {
			m.Source = "current"
			freeze = true
		}
		m.AfterUSD += after
		report.AfterUSD += after
		if !freeze || req.DryRun {
			continue
		}

		frozen := *c
		frozen.CostUSD = after
		delete(t.buckets, key)
		key.priced = true
		if existing := t.buckets[key]; existing != nil {
			existing.add(frozen)
		} else {
			t.buckets[key] = &frozen
		}
		report.Frozen++
	}
	if report.Frozen > 0 {
		t.dirty = true
	}

	for _, m := range models {
		report.Models = append(report.Models, *m)
	}
	slices.SortFunc(report.Models, func(a, b RepriceModel) int {
		if c := strings.Compare(a.Provider, b.Provider); c != 0 {
			return c
		}
		return strings.Compare(a.Model, b.Model)
	})
	return report
}

// pricePointAt returns the last point starting at or before at, or the first
// point when at predates the whole timeline. points must be sorted and non-empty.
func pricePointAt(points []PricePoint, at time.Time) PricePoint {
	chosen := points[0]
	for _, p := range points {
		if p.From.After(at) {
			break
		}
		chosen = p
	}
	return chosen
}
