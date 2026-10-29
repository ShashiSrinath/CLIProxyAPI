package usagestats

import (
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"time"
)

// CycleBounds finds calendar-aligned boundaries using the original anchor day.
func CycleBounds(now, anchor time.Time, cycle string, offset int, loc *time.Location) (time.Time, time.Time) {
	anchor = time.Date(anchor.In(loc).Year(), anchor.In(loc).Month(), anchor.In(loc).Day(), 0, 0, 0, 0, loc)
	boundary := func(n int) time.Time {
		if cycle == "weekly" {
			return anchor.AddDate(0, 0, n*7)
		}
		months := n
		if cycle == "yearly" {
			months *= 12
		}
		first := time.Date(anchor.Year(), anchor.Month()+time.Month(months), 1, 0, 0, 0, 0, loc)
		last := time.Date(first.Year(), first.Month()+1, 0, 0, 0, 0, 0, loc).Day()
		day := anchor.Day()
		if day > last {
			day = last
		}
		return time.Date(first.Year(), first.Month(), day, 0, 0, 0, 0, loc)
	}
	n := 0
	if cycle == "weekly" {
		n = int(now.Sub(anchor).Hours() / 24 / 7)
	} else {
		n = (now.In(loc).Year()-anchor.Year())*12 + int(now.In(loc).Month()-anchor.Month())
		if cycle == "yearly" {
			n /= 12
		}
	}
	for boundary(n).After(now) {
		n--
	}
	for !boundary(n + 1).After(now) {
		n++
	}
	n += offset
	return boundary(n), boundary(n + 1)
}

// PlanOwners gives explicit assignments precedence over provider-wide plans.
func PlanOwners(plans []config.SubscriptionPlan, providers map[string]string) (map[string]int, map[string]int) {
	owners := map[string]int{}
	legacy := map[string]int{}
	for i, p := range plans {
		for _, id := range p.Credentials {
			if _, ok := owners[id]; !ok {
				owners[id] = i
			}
		}
	}
	for i, p := range plans {
		if len(p.Credentials) == 0 {
			if _, ok := legacy[p.Provider]; !ok {
				legacy[p.Provider] = i
			}
		}
	}
	for id, provider := range providers {
		if _, ok := owners[id]; !ok {
			if i, ok := legacy[provider]; ok {
				owners[id] = i
			}
		}
	}
	return owners, legacy
}

func PlanMultiplier(value, price float64) *float64 {
	if price <= 0 {
		return nil
	}
	v := value / price
	return &v
}
func PlanProjection(value, last7, elapsed, length float64) float64 {
	if elapsed <= 0 {
		return 0
	}
	p := value / elapsed * length
	if elapsed >= 7 {
		p = (p + last7/7*length) / 2
	}
	return p
}
func WindowCapacity(value, fraction float64) (*float64, *float64) {
	if fraction < .05 {
		return nil, nil
	}
	capacity := value / fraction
	headroom := capacity * (1 - fraction)
	return &capacity, &headroom
}

// SummaryUnassigned includes unmatched credentials and legacy providers.
func (t *Tracker) SummaryUnassigned(q SummaryQuery, owners map[string]int, legacy map[string]int, prices PriceResolver) SummaryResponse {
	return t.summaryFiltered(q, nil, prices, func(row bucketRow) bool {
		if row.authID == "" {
			_, ok := legacy[row.provider]
			return !ok
		}
		_, ok := owners[row.authID]
		return !ok
	})
}

// UnassignedIdentities lists identities represented by unassigned usage buckets.
func (t *Tracker) UnassignedIdentities(q SummaryQuery, owners map[string]int, legacy map[string]int) map[string]string {
	rows, _ := t.rangeSnapshot(q.From.UTC().Truncate(time.Hour), q.To.UTC())
	identities := map[string]string{}
	for _, row := range rows {
		if row.authID == "" {
			if _, ok := legacy[row.provider]; !ok {
				identities["\x00"+row.provider] = row.provider
			}
			continue
		}
		if _, ok := owners[row.authID]; !ok {
			identities[row.authID] = row.provider
		}
	}
	return identities
}

// BreakEven returns the first bucket at which cumulative value covers the price.
func BreakEven(series []SeriesPoint, price float64) *string {
	if price <= 0 {
		return nil
	}
	sum := 0.0
	for _, point := range series {
		sum += point.CostUSD
		if sum >= price {
			t := point.T
			return &t
		}
	}
	return nil
}
