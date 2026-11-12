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
	Pricing       UsagePricingConfig `yaml:"pricing" json:"pricing"`
	Subscriptions []SubscriptionPlan `yaml:"subscriptions,omitempty" json:"subscriptions,omitempty"`
	// SmartRouting ranks credentials owned by subscription plans by their live
	// limit windows and overrides their scheduler priority. Nil means enabled.
	SmartRouting *bool `yaml:"smart-routing,omitempty" json:"smart-routing,omitempty"`
}

// SmartRoutingEnabled reports whether plan-aware credential ranking is active.
func (c UsageTrackingConfig) SmartRoutingEnabled() bool {
	return c.SmartRouting == nil || *c.SmartRouting
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
	// Tiers are time-of-day prices (for example peak/off-peak) that replace the
	// base price while their conditions match. Unset tier cache prices fall back
	// to the tier input price.
	Tiers []PriceTier `yaml:"tiers,omitempty" json:"tiers,omitempty"`
}

// PriceTier is a time-of-day price in USD per one million tokens. UTCStart and
// UTCEnd are HHMM in UTC (end exclusive; start > end wraps past midnight), and
// UTCDays lists lowercase weekday names. Empty conditions always match.
type PriceTier struct {
	UTCDays    []string `yaml:"utc-days,omitempty" json:"utc-days,omitempty"`
	UTCStart   *int     `yaml:"utc-start,omitempty" json:"utc-start,omitempty"`
	UTCEnd     *int     `yaml:"utc-end,omitempty" json:"utc-end,omitempty"`
	Input      float64  `yaml:"input" json:"input"`
	Output     float64  `yaml:"output" json:"output"`
	CacheRead  *float64 `yaml:"cache-read,omitempty" json:"cache-read,omitempty"`
	CacheWrite *float64 `yaml:"cache-write,omitempty" json:"cache-write,omitempty"`
}

// SubscriptionPlan describes a recurring subscription and its credentials.
type SubscriptionPlan struct {
	ID          string   `yaml:"id" json:"id"`
	Name        string   `yaml:"name" json:"name"`
	Provider    string   `yaml:"provider" json:"provider"`
	PriceUSD    float64  `yaml:"price-usd" json:"price-usd"`
	Cycle       string   `yaml:"cycle,omitempty" json:"cycle,omitempty"`
	AnchorDate  string   `yaml:"anchor-date,omitempty" json:"anchor-date,omitempty"`
	Credentials []string `yaml:"credentials,omitempty" json:"credentials,omitempty"`
	Notes       string   `yaml:"notes,omitempty" json:"notes,omitempty"`
	// LimitType is the plan's longest usage limit: "weekly" or "monthly". Empty auto-detects.
	LimitType string `yaml:"limit-type,omitempty" json:"limit-type,omitempty"`
	// Routing biases smart routing: "prefer" ranks first, "avoid" ranks last. Empty is automatic.
	Routing string `yaml:"routing,omitempty" json:"routing,omitempty"`
}
