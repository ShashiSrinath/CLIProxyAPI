package helps

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

// OpenCode Go exposes account-wide spend windows at {base}/usage. The endpoint is
// undocumented; its observed shape is:
//
//	{"usage":{"rolling":{"status":"ok","percent":8,"resetsAt":"..."},"weekly":{...},"monthly":{...}}}
const openCodeGoUsageMaxBody = 64 << 10

// OpenCodeGoUsageWindow is one spend window reported by the OpenCode Go usage endpoint.
type OpenCodeGoUsageWindow struct {
	Key      string
	Status   string
	Percent  float64
	ResetsAt time.Time
}

// Exhausted reports whether the window currently blocks requests.
func (w OpenCodeGoUsageWindow) Exhausted() bool {
	status := strings.ToLower(strings.TrimSpace(w.Status))
	return w.Percent >= 100 || (status != "" && status != "ok")
}

// OpenCodeGoDefaultHeaderAttrs returns the default OpenCode Go headers in auth attribute form,
// so they share the same $CPA-SESSION-ID expansion as user-configured headers.
func OpenCodeGoDefaultHeaderAttrs() map[string]string {
	return map[string]string{
		"header:User-Agent":         util.OpenCodeGoUserAgent(),
		"header:x-opencode-session": "$CPA-SESSION-ID",
	}
}

// ParseOpenCodeGoUsage extracts the known usage windows from a usage response body.
func ParseOpenCodeGoUsage(body []byte) []OpenCodeGoUsageWindow {
	usage := gjson.GetBytes(body, "usage")
	if !usage.IsObject() {
		return nil
	}
	windows := make([]OpenCodeGoUsageWindow, 0, len(util.OpenCodeGoUsageWindows))
	for _, known := range util.OpenCodeGoUsageWindows {
		item := usage.Get(known.Key)
		if !item.IsObject() {
			continue
		}
		window := OpenCodeGoUsageWindow{
			Key:     known.Key,
			Status:  item.Get("status").String(),
			Percent: item.Get("percent").Float(),
		}
		if resetsAt, errParse := time.Parse(time.RFC3339, item.Get("resetsAt").String()); errParse == nil {
			window.ResetsAt = resetsAt
		}
		windows = append(windows, window)
	}
	return windows
}

// OpenCodeGoQuotaCooldown returns how long the credential stays blocked: until the latest
// reset among exhausted windows. ok is false when no window is exhausted with a known reset.
func OpenCodeGoQuotaCooldown(windows []OpenCodeGoUsageWindow, now time.Time) (time.Duration, bool) {
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

// FetchOpenCodeGoUsage reads the usage windows for one API key.
func FetchOpenCodeGoUsage(ctx context.Context, client *http.Client, baseURL, apiKey string) ([]OpenCodeGoUsageWindow, error) {
	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, util.OpenCodeGoUsageURL(baseURL), nil)
	if errReq != nil {
		return nil, errReq
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", util.OpenCodeGoUserAgent())
	resp, errDo := client.Do(req)
	if errDo != nil {
		return nil, errDo
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("opencode go usage: close response body error: %v", errClose)
		}
	}()
	body, errRead := io.ReadAll(io.LimitReader(resp.Body, openCodeGoUsageMaxBody))
	if errRead != nil {
		return nil, errRead
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("opencode go usage: status %d", resp.StatusCode)
	}
	return ParseOpenCodeGoUsage(body), nil
}
