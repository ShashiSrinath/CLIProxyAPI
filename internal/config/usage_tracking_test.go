package config

import "testing"

func TestUsageTrackingV8Layout(t *testing.T) {
	data := []byte(`config-version: 8
access:
  api-keys: [sk-abcdefghijkl]
  api-key-names: { sk-abcdefghijkl: Laptop }
observability:
  usage:
    tracking:
      retention-days: 30
      pricing:
        overrides:
          "claude-*": { openrouter-id: "anthropic/claude-sonnet-4.5", input: 1.5 }
`)
	if err := ValidateV8Config(data); err != nil {
		t.Fatal(err)
	}
	cfg, err := ParseConfigBytes(data)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.UsageTracking.RetentionDays != 30 || cfg.APIKeyNames["sk-abcdefghijkl"] != "Laptop" || *cfg.UsageTracking.Pricing.Overrides["claude-*"].Input != 1.5 {
		t.Fatalf("%+v %+v", cfg.UsageTracking, cfg.APIKeyNames)
	}
}
