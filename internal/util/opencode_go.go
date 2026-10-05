package util

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/buildinfo"
)

const openCodeGoHostPath = "opencode.ai/zen/go"

// OpenCodeGoUsageWindows lists the spend windows reported at {base}/usage with display labels.
var OpenCodeGoUsageWindows = []struct{ Key, Label string }{
	{Key: "rolling", Label: "5h"},
	{Key: "weekly", Label: "weekly"},
	{Key: "monthly", Label: "monthly"},
}

// IsOpenCodeGoBaseURL reports whether baseURL points at the OpenCode Go subscription API.
func IsOpenCodeGoBaseURL(baseURL string) bool {
	return strings.Contains(strings.ToLower(baseURL), openCodeGoHostPath)
}

// OpenCodeGoUsageURL returns the account usage endpoint for an OpenCode Go base URL.
func OpenCodeGoUsageURL(baseURL string) string {
	return strings.TrimSuffix(strings.TrimSpace(baseURL), "/") + "/usage"
}

// OpenCodeGoUserAgent identifies the proxy to OpenCode Go, which rejects generic SDK user agents.
func OpenCodeGoUserAgent() string {
	return "cli-proxy-api/" + buildinfo.Version
}

// OpenCodeGoQuotaProbe returns the management quota probe for an OpenCode Go credential.
// The usage endpoint reports percent used and the reset time per spend window.
func OpenCodeGoQuotaProbe(baseURL string) map[string]any {
	buckets := make([]any, 0, len(OpenCodeGoUsageWindows))
	for _, window := range OpenCodeGoUsageWindows {
		buckets = append(buckets, map[string]any{
			"window":       window.Label,
			"used_percent": "usage." + window.Key + ".percent",
			"reset_time":   "usage." + window.Key + ".resetsAt",
			"description":  "usage." + window.Key + ".status",
		})
	}
	return map[string]any{
		"url": OpenCodeGoUsageURL(baseURL),
		"headers": map[string]any{
			"Authorization": "Bearer $TOKEN$",
			"Accept":        "application/json",
			"User-Agent":    OpenCodeGoUserAgent(),
		},
		"mapping": map[string]any{
			"groups": []any{
				map[string]any{"display_name": "OpenCode Go", "buckets": buckets},
			},
		},
	}
}
