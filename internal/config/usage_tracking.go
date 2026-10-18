package config

// UsageTrackingConfig configures the persistent usage aggregator that records
// requests and tokens per client API key and model.
type UsageTrackingConfig struct {
	// Disabled turns usage tracking off. Tracking is on by default.
	Disabled bool `yaml:"disabled,omitempty" json:"disabled,omitempty"`
	// RetentionDays bounds how long aggregated usage is kept. 0 uses the default (90).
	RetentionDays int `yaml:"retention-days,omitempty" json:"retention-days,omitempty"`
	// DataDir overrides where usage data and the pricing cache are stored.
	// Empty uses "<auth-dir>/usage".
	DataDir string `yaml:"data-dir,omitempty" json:"data-dir,omitempty"`
	// Pricing configures model pricing used to estimate cost.
	Pricing UsagePricingConfig `yaml:"pricing" json:"pricing"`
}

// UsagePricingConfig configures where model prices come from.
type UsagePricingConfig struct {
	// OpenRouterDisabled stops the automatic price catalog fetch from OpenRouter.
	OpenRouterDisabled bool `yaml:"openrouter-disabled,omitempty" json:"openrouter-disabled,omitempty"`
	// RefreshHours controls how often the OpenRouter catalog is refetched. 0 uses the default (24).
	RefreshHours int `yaml:"refresh-hours,omitempty" json:"refresh-hours,omitempty"`
	// Overrides maps a model name or glob pattern (path.Match syntax) to a price override.
	Overrides map[string]ModelPriceOverride `yaml:"overrides,omitempty" json:"overrides,omitempty"`
}

// ModelPriceOverride either maps a model to a specific OpenRouter model id or
// sets manual prices in USD per one million tokens. Manual prices win over
// OpenRouterID when both are set; unset manual fields fall back to the
// OpenRouter price (when OpenRouterID resolves) or to the input price.
type ModelPriceOverride struct {
	OpenRouterID string   `yaml:"openrouter-id,omitempty" json:"openrouter-id,omitempty"`
	Input        *float64 `yaml:"input,omitempty" json:"input,omitempty"`
	Output       *float64 `yaml:"output,omitempty" json:"output,omitempty"`
	CacheRead    *float64 `yaml:"cache-read,omitempty" json:"cache-read,omitempty"`
	CacheWrite   *float64 `yaml:"cache-write,omitempty" json:"cache-write,omitempty"`
}
