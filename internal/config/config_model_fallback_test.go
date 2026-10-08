package config

import "testing"

func TestV8RoutingModelFallbackLoads(t *testing.T) {
	data := []byte(`config-version: 8
routing:
  model-fallback:
    enabled: true
    rules:
      - model: "gpt-6.1-sol"
        fallback-model: "deepseek-v4.1-flash"
        fallback-providers: ["opencode-go", "commandcode"]
        fallback-reasoning-effort: "high"
`)
	if err := ValidateV8Config(data); err != nil {
		t.Fatalf("ValidateV8Config: %v", err)
	}
	cfg, err := ParseConfigBytes(data)
	if err != nil {
		t.Fatalf("ParseConfigBytes: %v", err)
	}
	fallback := cfg.ModelFallback
	if !fallback.Enabled || len(fallback.Rules) != 1 {
		t.Fatalf("ModelFallback = %+v", fallback)
	}
	rule := fallback.Rules[0]
	if rule.Model != "gpt-6.1-sol" || rule.FallbackModel != "deepseek-v4.1-flash" || rule.FallbackReasoningEffort != "high" ||
		len(rule.FallbackProviders) != 2 || rule.FallbackProviders[1] != "commandcode" {
		t.Fatalf("rule = %+v", rule)
	}
}

func TestLegacyModelFallbackMigratesToRouting(t *testing.T) {
	data := []byte(`model-fallback:
  enabled: true
  rules:
    - model: "gpt-6.1-sol"
      fallback-model: "deepseek-v4.1-flash"
`)
	normalized, _, err := NormalizeConfigLayout(data, true)
	if err != nil {
		t.Fatalf("NormalizeConfigLayout: %v", err)
	}
	if err = ValidateV8Config(normalized); err != nil {
		t.Fatalf("ValidateV8Config: %v\n%s", err, normalized)
	}
	cfg, err := ParseConfigBytes(normalized)
	if err != nil {
		t.Fatalf("ParseConfigBytes: %v", err)
	}
	if !cfg.ModelFallback.Enabled || len(cfg.ModelFallback.Rules) != 1 {
		t.Fatalf("ModelFallback = %+v\n%s", cfg.ModelFallback, normalized)
	}
}
