package helps

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

// Command Code exposes account credits and rolling spend windows at
// {host}/alpha/billing/credits. The endpoint is undocumented; its observed shape is:
//
//	{"credits":{"monthlyCredits":69.9,"purchasedCredits":0},"windowLimits":{
//	  "limited":true,"fiveHour":{"used":0.08,"cap":14,"exceeded":false,"resetAt":1791256488371},
//	  "weekly":{"used":0.08,"cap":35,"exceeded":false,"resetAt":1791843288371}}}
//
// resetAt is epoch milliseconds. used/cap are credit-value dollars.
const commandCodeUsageMaxBody = 64 << 10

// CommandCodeUsageWindow is one rolling spend window from the Command Code credits endpoint.
type CommandCodeUsageWindow struct {
	Key      string
	Used     float64
	Cap      float64
	Exceeded bool
	ResetsAt time.Time
}

// Exhausted reports whether the window currently blocks requests.
func (w CommandCodeUsageWindow) Exhausted() bool {
	if w.Exceeded {
		return true
	}
	return w.Cap > 0 && w.Used >= w.Cap
}

// ParseCommandCodeUsage extracts the known rolling windows from a credits response body.
func ParseCommandCodeUsage(body []byte) []CommandCodeUsageWindow {
	limits := gjson.GetBytes(body, "windowLimits")
	if !limits.IsObject() {
		return nil
	}
	windows := make([]CommandCodeUsageWindow, 0, len(util.CommandCodeUsageWindows))
	for _, known := range util.CommandCodeUsageWindows {
		item := limits.Get(known.Key)
		if !item.IsObject() {
			continue
		}
		window := CommandCodeUsageWindow{
			Key:      known.Key,
			Used:     item.Get("used").Float(),
			Cap:      item.Get("cap").Float(),
			Exceeded: item.Get("exceeded").Bool(),
		}
		if resetsAt := item.Get("resetAt"); resetsAt.Exists() {
			if ms := resetsAt.Int(); ms > 0 {
				window.ResetsAt = time.UnixMilli(ms)
			}
		}
		windows = append(windows, window)
	}
	return windows
}

// CommandCodeQuotaCooldown returns how long the credential stays blocked: until the latest
// reset among exhausted windows. ok is false when no window is exhausted with a known reset.
func CommandCodeQuotaCooldown(windows []CommandCodeUsageWindow, now time.Time) (time.Duration, bool) {
	var latest time.Time
	for _, window := range windows {
		if window.Exhausted() && window.ResetsAt.After(latest) {
			latest = window.ResetsAt
		}
	}
	if latest.IsZero() || !latest.After(now) {
		return 0, false
	}
	return latest.Sub(now), true
}

// FetchCommandCodeUsage reads the rolling usage windows for one API key.
func FetchCommandCodeUsage(ctx context.Context, client *http.Client, baseURL, apiKey string) ([]CommandCodeUsageWindow, error) {
	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, util.CommandCodeCreditsURL(baseURL), nil)
	if errReq != nil {
		return nil, errReq
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")
	resp, errDo := client.Do(req)
	if errDo != nil {
		return nil, errDo
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("command code usage: close response body error: %v", errClose)
		}
	}()
	body, errRead := io.ReadAll(io.LimitReader(resp.Body, commandCodeUsageMaxBody))
	if errRead != nil {
		return nil, errRead
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("command code usage: status %d", resp.StatusCode)
	}
	return ParseCommandCodeUsage(body), nil
}
