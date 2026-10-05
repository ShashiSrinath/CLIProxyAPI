package management

import (
	"math"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
)

func TestMapProbeResponse_CommandCodeUsedAmount(t *testing.T) {
	// Captured from https://api.commandcode.ai/alpha/billing/credits.
	body := []byte(`{"credits":{"monthlyCredits":69.9177663613,"purchasedCredits":0,"freeCredits":0},"windowLimits":{"limited":true,"fiveHour":{"used":0.0822336387,"cap":14,"exceeded":false,"resetAt":1791256488371},"weekly":{"used":0.0822336387,"cap":35,"exceeded":false,"resetAt":1791843288371}}}`)
	probe := util.CommandCodeQuotaProbe("https://api.commandcode.ai/provider/v1")
	mapping, _ := probe["mapping"].(map[string]any)
	resp, err := mapProbeResponse(body, mapping)
	if err != nil {
		t.Fatalf("mapProbeResponse() error = %v", err)
	}
	if len(resp.Groups) != 1 || len(resp.Groups[0].Buckets) != 2 {
		t.Fatalf("groups = %+v", resp.Groups)
	}
	fiveHour := resp.Groups[0].Buckets[0]
	wantFiveHour := 1 - 0.0822336387/14
	if fiveHour.Window != "5h" || math.Abs(fiveHour.RemainingFraction-wantFiveHour) > 1e-9 || fiveHour.ResetTime != "1791256488371" {
		t.Fatalf("fiveHour bucket = %+v", fiveHour)
	}
	weekly := resp.Groups[0].Buckets[1]
	wantWeekly := 1 - 0.0822336387/35
	if weekly.Window != "weekly" || math.Abs(weekly.RemainingFraction-wantWeekly) > 1e-9 {
		t.Fatalf("weekly bucket = %+v", weekly)
	}
}
