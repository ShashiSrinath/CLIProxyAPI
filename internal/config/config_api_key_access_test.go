package config

import (
	"strings"
	"testing"
)

func TestV8AccessAPIKeyModelsAndLimitsLoads(t *testing.T) {
	data := []byte(`config-version: 8
access:
  api-keys:
    - "client-a"
    - "client.b"
  api-key-models:
    "client.b": ["deepseek-*", " kimi-k2* ", ""]
  api-key-limits:
    "client.b":
      daily-usd: 5
      weekly-usd: 20
      monthly-usd: 60
      timezone: "Asia/Kolkata"
`)
	if err := ValidateV8Config(data); err != nil {
		t.Fatalf("ValidateV8Config: %v", err)
	}
	cfg, err := ParseConfigBytes(data)
	if err != nil {
		t.Fatalf("ParseConfigBytes: %v", err)
	}
	got := cfg.APIKeyModels["client.b"]
	if len(got) != 2 || got[0] != "deepseek-*" || got[1] != "kimi-k2*" {
		t.Fatalf("APIKeyModels[client.b] = %v", got)
	}
	if _, ok := cfg.APIKeyModels["client-a"]; ok {
		t.Fatal("unrestricted key must not get a models entry")
	}
	limit, ok := cfg.APIKeyLimits["client.b"]
	if !ok {
		t.Fatal("APIKeyLimits[client.b] missing")
	}
	if limit.DailyUSD != 5 || limit.WeeklyUSD != 20 || limit.MonthlyUSD != 60 || limit.Timezone != "Asia/Kolkata" {
		t.Fatalf("APIKeyLimits[client.b] = %+v", limit)
	}
	if !limit.HasCap() {
		t.Fatal("limit with positive amounts must report a cap")
	}
	if (APIKeyLimit{Timezone: "UTC"}).HasCap() {
		t.Fatal("limit without amounts must not report a cap")
	}
}

func TestLegacyAPIKeyModelsAndLimitsMigrateToAccess(t *testing.T) {
	data := []byte(`api-keys:
  - "client-a"
api-key-models:
  "client-a": ["gemini-*"]
api-key-limits:
  "client-a": { daily-usd: 1.5 }
`)
	normalized, changed, err := NormalizeConfigLayout(data, true)
	if err != nil {
		t.Fatalf("NormalizeConfigLayout: %v", err)
	}
	if !changed {
		t.Fatal("legacy fields should be migrated")
	}
	if err = ValidateV8Config(normalized); err != nil {
		t.Fatalf("ValidateV8Config: %v\n%s", err, normalized)
	}
	if strings.Contains(string(normalized), "\napi-key-models:") || strings.Contains(string(normalized), "\napi-key-limits:") {
		t.Fatalf("legacy roots were not moved:\n%s", normalized)
	}
	cfg, err := ParseConfigBytes(normalized)
	if err != nil {
		t.Fatalf("ParseConfigBytes: %v", err)
	}
	if got := cfg.APIKeyModels["client-a"]; len(got) != 1 || got[0] != "gemini-*" {
		t.Fatalf("APIKeyModels = %v\n%s", cfg.APIKeyModels, normalized)
	}
	if limit := cfg.APIKeyLimits["client-a"]; limit.DailyUSD != 1.5 {
		t.Fatalf("APIKeyLimits = %v\n%s", cfg.APIKeyLimits, normalized)
	}
}

func TestAPIKeyAccessValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		data string
		want string
	}{
		{"negative daily", `access: {api-keys: [k], api-key-limits: {k: {daily-usd: -1}}}`, "must not be negative"},
		{"negative weekly", `access: {api-keys: [k], api-key-limits: {k: {weekly-usd: -0.01}}}`, "must not be negative"},
		{"negative monthly", `access: {api-keys: [k], api-key-limits: {k: {monthly-usd: -5}}}`, "must not be negative"},
		{"bad timezone", `access: {api-keys: [k], api-key-limits: {k: {monthly-usd: 5, timezone: Mars/Olympus}}}`, "timezone"},
		{"valid", `access: {api-keys: [k], api-key-limits: {k: {monthly-usd: 5, timezone: Asia/Kolkata}}}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseConfigBytes([]byte("config-version: 8\n" + tc.data + "\n"))
			if tc.want == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestSanitizeAPIKeyModelsDropsEmptyPatterns(t *testing.T) {
	cfg := &Config{}
	cfg.APIKeyModels = map[string][]string{
		" a ": {" m1 ", "", "m1", "  "},
		"b":   {"", " "},
		"":    {"x"},
	}
	cfg.SanitizeAPIKeyModels()
	if got := cfg.APIKeyModels["a"]; len(got) != 1 || got[0] != "m1" {
		t.Fatalf("APIKeyModels[a] = %v", got)
	}
	if _, ok := cfg.APIKeyModels["b"]; ok {
		t.Fatalf("key without usable pattern must be dropped: %v", cfg.APIKeyModels)
	}
	if _, ok := cfg.APIKeyModels[""]; ok {
		t.Fatalf("blank key must be dropped: %v", cfg.APIKeyModels)
	}
}
