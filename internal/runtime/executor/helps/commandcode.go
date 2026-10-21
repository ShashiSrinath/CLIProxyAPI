package helps

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
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
//
// commandCodeUsageMaxBody bounds every billing endpoint response read.
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
	body, errFetch := fetchCommandCodeBilling(ctx, client, util.CommandCodeCreditsURL(baseURL), apiKey)
	if errFetch != nil {
		return nil, fmt.Errorf("command code usage: %w", errFetch)
	}
	return ParseCommandCodeUsage(body), nil
}

// Command Code rejects generation with HTTP 400 when the account balance cannot cover
// the request, even while the balance is still slightly positive:
//
//	{"error":{"message":"You have insufficient credits to make this request. Please purchase
//	  more credits to continue using the service.","type":"invalid_request_error","code":"BAD_REQUEST"}}
//
// Monthly plan credits refill at the subscription's currentPeriodEnd, reported by
// {host}/alpha/billing/subscriptions (also undocumented):
//
//	{"success":true,"data":{"status":"active","currentPeriodStart":"2026-10-05T22:08:39.000Z",
//	  "currentPeriodEnd":"2026-11-05T22:08:39.000Z","planId":"individual-goat",...}}
const (
	// CommandCodeCreditsFallbackCooldown applies when the refill time is unknown. Purchased
	// top-ups can land at any moment, so an out-of-credits key is re-probed hourly; the
	// probe costs one rejected request that immediately fails over to another credential.
	CommandCodeCreditsFallbackCooldown = time.Hour
	// CommandCodeCreditsMaxCooldown caps the wait for the monthly refill. The period end
	// can be weeks away, and blocking a key that long would ignore a manual top-up; six
	// hours keeps the re-probe cost negligible while noticing a top-up the same day.
	CommandCodeCreditsMaxCooldown = 6 * time.Hour
)

const commandCodeInsufficientCreditsPhrase = "insufficient credits"

// IsCommandCodeInsufficientCredits reports whether a Command Code error response means the
// account ran out of credits. Only 400/402 bodies whose error message names insufficient
// credits match, so ordinary bad-request errors keep their request-fault semantics.
func IsCommandCodeInsufficientCredits(status int, body []byte) bool {
	if status != http.StatusBadRequest && status != http.StatusPaymentRequired {
		return false
	}
	message := commandCodeErrorMessage(body)
	return strings.Contains(strings.ToLower(message), commandCodeInsufficientCreditsPhrase)
}

// CommandCodeInsufficientCreditsBody rewrites the upstream rejection into the OpenAI
// insufficient_quota error shape that accompanies HTTP 429, keeping the upstream message.
func CommandCodeInsufficientCreditsBody(body []byte) []byte {
	message := strings.TrimSpace(commandCodeErrorMessage(body))
	if message == "" {
		message = "Command Code: insufficient credits"
	}
	payload, errMarshal := json.Marshal(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    "insufficient_quota",
			"code":    "insufficient_quota",
		},
	})
	if errMarshal != nil {
		return body
	}
	return payload
}

func commandCodeErrorMessage(body []byte) string {
	if message := gjson.GetBytes(body, "error.message"); message.Type == gjson.String {
		return message.String()
	}
	if message := gjson.GetBytes(body, "message"); message.Type == gjson.String {
		return message.String()
	}
	return ""
}

// ParseCommandCodeSubscriptionPeriodEnd extracts data.currentPeriodEnd from a subscriptions
// response body. ok is false when the field is missing or not an RFC 3339 timestamp.
func ParseCommandCodeSubscriptionPeriodEnd(body []byte) (time.Time, bool) {
	raw := strings.TrimSpace(gjson.GetBytes(body, "data.currentPeriodEnd").String())
	if raw == "" {
		return time.Time{}, false
	}
	periodEnd, errParse := time.Parse(time.RFC3339Nano, raw)
	if errParse != nil {
		return time.Time{}, false
	}
	return periodEnd, true
}

// CommandCodeCreditsCooldown returns how long an out-of-credits credential stays blocked:
// until the monthly refill at periodEnd, capped at CommandCodeCreditsMaxCooldown, or
// CommandCodeCreditsFallbackCooldown when periodEnd is unknown or already past.
func CommandCodeCreditsCooldown(periodEnd, now time.Time) time.Duration {
	if periodEnd.IsZero() || !periodEnd.After(now) {
		return CommandCodeCreditsFallbackCooldown
	}
	return min(periodEnd.Sub(now), CommandCodeCreditsMaxCooldown)
}

// FetchCommandCodeSubscriptionPeriodEnd reads when the monthly plan credits refill for one API key.
func FetchCommandCodeSubscriptionPeriodEnd(ctx context.Context, client *http.Client, baseURL, apiKey string) (time.Time, error) {
	body, errFetch := fetchCommandCodeBilling(ctx, client, util.CommandCodeSubscriptionsURL(baseURL), apiKey)
	if errFetch != nil {
		return time.Time{}, fmt.Errorf("command code subscription: %w", errFetch)
	}
	periodEnd, ok := ParseCommandCodeSubscriptionPeriodEnd(body)
	if !ok {
		return time.Time{}, fmt.Errorf("command code subscription: missing currentPeriodEnd")
	}
	return periodEnd, nil
}

// fetchCommandCodeBilling GETs one billing endpoint and returns its size-limited body.
func fetchCommandCodeBilling(ctx context.Context, client *http.Client, endpoint, apiKey string) ([]byte, error) {
	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
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
			log.Errorf("command code billing: close response body error: %v", errClose)
		}
	}()
	body, errRead := io.ReadAll(io.LimitReader(resp.Body, commandCodeUsageMaxBody))
	if errRead != nil {
		return nil, errRead
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	return body, nil
}
