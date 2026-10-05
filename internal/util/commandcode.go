package util

import (
	"net/url"
	"strings"
)

const commandCodeHostPath = "api.commandcode.ai"

// commandCodeCreditsPath is the account credits/usage endpoint. It lives on the API
// host root, not under the /provider/v1 base path used for generation.
const commandCodeCreditsPath = "/alpha/billing/credits"

// CommandCodeUsageWindows lists the rolling spend windows reported by the Command
// Code credits endpoint. Keys match the upstream "windowLimits" JSON fields.
var CommandCodeUsageWindows = []struct{ Key, Label string }{
	{Key: "fiveHour", Label: "5h"},
	{Key: "weekly", Label: "weekly"},
}

// IsCommandCodeBaseURL reports whether baseURL points at the Command Code provider API.
func IsCommandCodeBaseURL(baseURL string) bool {
	return strings.Contains(strings.ToLower(baseURL), commandCodeHostPath)
}

// CommandCodeCreditsURL returns the account credits endpoint for a Command Code base URL.
func CommandCodeCreditsURL(baseURL string) string {
	if parsed, errParse := url.Parse(strings.TrimSpace(baseURL)); errParse == nil && parsed.Host != "" {
		scheme := parsed.Scheme
		if scheme == "" {
			scheme = "https"
		}
		return scheme + "://" + parsed.Host + commandCodeCreditsPath
	}
	return "https://" + commandCodeHostPath + commandCodeCreditsPath
}

// CommandCodeQuotaProbe returns the management quota probe for a Command Code credential.
// The credits endpoint reports used/cap and the reset time per rolling spend window.
func CommandCodeQuotaProbe(baseURL string) map[string]any {
	buckets := make([]any, 0, len(CommandCodeUsageWindows))
	for _, window := range CommandCodeUsageWindows {
		buckets = append(buckets, map[string]any{
			"window":       window.Label,
			"used_amount":  "windowLimits." + window.Key + ".used",
			"total_amount": "windowLimits." + window.Key + ".cap",
			"reset_time":   "windowLimits." + window.Key + ".resetAt",
		})
	}
	return map[string]any{
		"url": CommandCodeCreditsURL(baseURL),
		"headers": map[string]any{
			"Authorization": "Bearer $TOKEN$",
			"Accept":        "application/json",
		},
		"mapping": map[string]any{
			"groups": []any{
				map[string]any{"display_name": "Command Code", "buckets": buckets},
			},
		},
	}
}
