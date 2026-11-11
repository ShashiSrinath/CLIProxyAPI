package handlers

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

// ModelAllowList is the set of model name patterns a client API key may use.
// A nil list means the key is unrestricted.
type ModelAllowList []string

// ModelAllowListForKey returns the model allow-list configured for a client API key.
// Patterns are matched case-insensitively with path.Match semantics.
func ModelAllowListForKey(cfg *config.SDKConfig, apiKey string) ModelAllowList {
	if cfg == nil || len(cfg.APIKeyModels) == 0 {
		return nil
	}
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return nil
	}
	entries, ok := cfg.APIKeyModels[apiKey]
	if !ok {
		return nil
	}
	allowed := make(ModelAllowList, 0, len(entries))
	for _, entry := range entries {
		if entry = strings.ToLower(strings.TrimSpace(entry)); entry != "" {
			allowed = append(allowed, entry)
		}
	}
	if len(allowed) == 0 {
		return nil
	}
	return allowed
}

// ModelAllowListForRequest returns the model allow-list of the client API key that
// authenticated the request.
func ModelAllowListForRequest(cfg *config.SDKConfig, c *gin.Context) ModelAllowList {
	if c == nil {
		return nil
	}
	value, exists := c.Get("userApiKey")
	if !exists || value == nil {
		return nil
	}
	return ModelAllowListForKey(cfg, fmt.Sprint(value))
}

func modelAllowListFromContext(cfg *config.SDKConfig, ctx context.Context) ModelAllowList {
	if ctx == nil {
		return nil
	}
	ginCtx, _ := ctx.Value("gin").(*gin.Context)
	return ModelAllowListForRequest(cfg, ginCtx)
}

// Allows reports whether the model may be used. The thinking suffix is stripped
// before matching, and a model served under a "prefix/" also matches the bare
// name so a pattern like "claude-*" matches "team/claude-sonnet-4-5".
func (l ModelAllowList) Allows(model string) bool {
	if l == nil {
		return true
	}
	name := normalizeAllowedModelName(model)
	if name == "" {
		return false
	}
	candidates := []string{name}
	if slash := strings.LastIndex(name, "/"); slash >= 0 && slash+1 < len(name) {
		candidates = append(candidates, name[slash+1:])
	}
	for _, pattern := range l {
		for _, candidate := range candidates {
			matched, err := path.Match(pattern, candidate)
			if err != nil {
				continue
			}
			if matched {
				return true
			}
		}
	}
	return false
}

// Filter returns the models that may be used, preserving order.
func (l ModelAllowList) Filter(models []string) []string {
	if l == nil {
		return models
	}
	out := make([]string, 0, len(models))
	for _, model := range models {
		if l.Allows(model) {
			out = append(out, model)
		}
	}
	return out
}

// checkModelAccess rejects models the client API key may not use.
func (h *BaseAPIHandler) checkModelAccess(ctx context.Context, modelName string) *interfaces.ErrorMessage {
	if h == nil {
		return nil
	}
	if modelAllowListFromContext(h.Cfg, ctx).Allows(modelName) {
		return nil
	}
	return providerNotAllowedError(modelName)
}

// normalizeAllowedModelName strips the thinking suffix, surrounding whitespace and
// lowercases the model name so patterns compare case-insensitively.
func normalizeAllowedModelName(model string) string {
	name := thinking.ParseSuffix(strings.TrimSpace(model)).ModelName
	return strings.ToLower(strings.TrimSpace(name))
}
