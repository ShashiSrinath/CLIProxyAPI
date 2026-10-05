package handlers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/sjson"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

const openAICompatibilityProviderFamily = "openai-compatibility"

// ProviderAllowList is the set of upstream providers a client API key may use.
// A nil list means the key is unrestricted.
type ProviderAllowList []string

// ProviderAllowListForKey returns the provider allow-list configured for a client API key.
func ProviderAllowListForKey(cfg *config.SDKConfig, apiKey string) ProviderAllowList {
	if cfg == nil || len(cfg.APIKeyProviders) == 0 {
		return nil
	}
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return nil
	}
	entries, ok := cfg.APIKeyProviders[apiKey]
	if !ok {
		return nil
	}
	allowed := make(ProviderAllowList, 0, len(entries))
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

// ProviderAllowListForRequest returns the provider allow-list of the client API key that
// authenticated the request.
func ProviderAllowListForRequest(cfg *config.SDKConfig, c *gin.Context) ProviderAllowList {
	if c == nil {
		return nil
	}
	value, exists := c.Get("userApiKey")
	if !exists || value == nil {
		return nil
	}
	return ProviderAllowListForKey(cfg, fmt.Sprint(value))
}

func providerAllowListFromContext(cfg *config.SDKConfig, ctx context.Context) ProviderAllowList {
	if ctx == nil {
		return nil
	}
	ginCtx, _ := ctx.Value("gin").(*gin.Context)
	return ProviderAllowListForRequest(cfg, ginCtx)
}

// Allows reports whether the provider may be used. Entries match provider ids exactly, an
// openai-compatibility name matches its internal provider key, and "openai-compatibility"
// matches every openai-compatibility provider.
func (l ProviderAllowList) Allows(provider string) bool {
	if l == nil {
		return true
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		return false
	}
	for _, entry := range l {
		if entry == provider || util.OpenAICompatibleProviderKey(entry) == provider {
			return true
		}
		if entry == openAICompatibilityProviderFamily && util.OpenAICompatibleProviderKey(provider) == provider {
			return true
		}
	}
	return false
}

// Filter returns the providers that may be used, preserving order.
func (l ProviderAllowList) Filter(providers []string) []string {
	if l == nil {
		return providers
	}
	out := make([]string, 0, len(providers))
	for _, provider := range providers {
		if l.Allows(provider) {
			out = append(out, provider)
		}
	}
	return out
}

// restrictProvidersForRequest drops providers the client API key may not use.
func (h *BaseAPIHandler) restrictProvidersForRequest(ctx context.Context, providers []string, modelName string) ([]string, *interfaces.ErrorMessage) {
	if h == nil {
		return providers, nil
	}
	allowed := providerAllowListFromContext(h.Cfg, ctx)
	if allowed == nil {
		return providers, nil
	}
	filtered := allowed.Filter(providers)
	if len(filtered) == 0 {
		return nil, providerNotAllowedError(modelName)
	}
	return filtered, nil
}

// checkPluginExecutorAccess rejects model-router plugin executors the client API key may not use.
func (h *BaseAPIHandler) checkPluginExecutorAccess(ctx context.Context, executorPluginID, modelName string) *interfaces.ErrorMessage {
	if h == nil {
		return nil
	}
	if providerAllowListFromContext(h.Cfg, ctx).Allows(executorPluginID) {
		return nil
	}
	return providerNotAllowedError(modelName)
}

func providerNotAllowedError(modelName string) *interfaces.ErrorMessage {
	body := `{"error":{"message":"","type":"permission_error","code":"model_not_allowed","param":"model"}}`
	body, errSet := sjson.Set(body, "error.message", "API key is not allowed to use model "+modelName)
	if errSet != nil {
		body = `{"error":{"message":"API key is not allowed to use this model","type":"permission_error","code":"model_not_allowed","param":"model"}}`
	}
	return &interfaces.ErrorMessage{
		StatusCode: http.StatusForbidden,
		Error:      errors.New(body),
	}
}

// FilterModelsForRequest removes models that no provider allowed for the client API key serves.
func (h *BaseAPIHandler) FilterModelsForRequest(c *gin.Context, models []map[string]any) []map[string]any {
	if h == nil {
		return models
	}
	allowed := ProviderAllowListForRequest(h.Cfg, c)
	if allowed == nil {
		return models
	}
	modelRegistry := registry.GetGlobalRegistry()
	out := make([]map[string]any, 0, len(models))
	for _, model := range models {
		id, _ := model["id"].(string)
		if id == "" {
			name, _ := model["name"].(string)
			id = strings.TrimPrefix(name, "models/")
		}
		if id != "" && len(allowed.Filter(modelRegistry.GetModelProviders(id))) > 0 {
			out = append(out, model)
		}
	}
	return out
}
