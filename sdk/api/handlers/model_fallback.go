package handlers

import (
	"maps"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/context"
)

// modelFallback is the execution target used once a main model is out of limits.
type modelFallback struct {
	providers   []string
	model       string
	handlerType string
}

// modelFallbackFor returns the configured fallback for currentModel when err shows the main
// model is out of limits. Only HTTP 429 qualifies: the auth manager reports it after every
// eligible credential was tried or while all credentials are cooling down.
func (h *BaseAPIHandler) modelFallbackFor(ctx context.Context, handlerType, currentModel string, err error) (modelFallback, bool) {
	if h == nil || h.Cfg == nil || !h.Cfg.ModelFallback.Enabled || err == nil {
		return modelFallback{}, false
	}
	if ctx != nil && ctx.Err() != nil {
		return modelFallback{}, false
	}
	if statusFromError(err) != http.StatusTooManyRequests {
		return modelFallback{}, false
	}
	baseModel := strings.TrimSpace(thinking.ParseSuffix(currentModel).ModelName)
	if baseModel == "" {
		return modelFallback{}, false
	}
	for _, rule := range h.Cfg.ModelFallback.Rules {
		if !strings.EqualFold(strings.TrimSpace(rule.Model), baseModel) {
			continue
		}
		target := strings.TrimSpace(rule.FallbackModel)
		if target == "" || strings.EqualFold(strings.TrimSpace(thinking.ParseSuffix(target).ModelName), baseModel) {
			return modelFallback{}, false
		}
		effort := strings.ToLower(strings.TrimSpace(rule.FallbackReasoningEffort))
		if effort != "" && !thinking.ParseSuffix(target).HasSuffix {
			target += "(" + effort + ")"
		}
		providers, normalizedModel, errMsg := h.getRequestDetailsWithOptions(target, false)
		if errMsg != nil {
			log.Warnf("model fallback: %s is out of limits but fallback model %s cannot be routed", baseModel, target)
			return modelFallback{}, false
		}
		if allowed := fallbackProviderAllowList(rule.FallbackProviders); allowed != nil {
			providers = allowed.Filter(providers)
		}
		providers = adjustExecutionProvidersForEntryProtocol(handlerType, providers)
		if providers, errMsg = h.restrictProvidersForRequest(ctx, providers, target); errMsg != nil || len(providers) == 0 {
			log.Warnf("model fallback: %s is out of limits but no allowed provider serves fallback model %s", baseModel, target)
			return modelFallback{}, false
		}
		log.Infof("model fallback: %s is out of limits, rerouting to %s via %s", baseModel, normalizedModel, strings.Join(providers, ","))
		return modelFallback{
			providers:   providers,
			model:       normalizedModel,
			handlerType: handlerType,
		}, true
	}
	return modelFallback{}, false
}

// apply retargets the request at the fallback model. Metadata is cloned so the main
// attempt's options stay untouched.
func (f modelFallback) apply(req coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Request, coreexecutor.Options) {
	req.Model = f.model
	meta := maps.Clone(opts.Metadata)
	if meta == nil {
		meta = make(map[string]any, 2)
	}
	meta[coreexecutor.RequestedModelMetadataKey] = f.model
	// A pinned main-model credential cannot serve the fallback providers.
	delete(meta, coreexecutor.PinnedAuthMetadataKey)
	delete(meta, coreexecutor.AuthSelectionModelMetadataKey)
	delete(meta, coreexecutor.ReasoningEffortMetadataKey)
	setReasoningEffortMetadata(meta, f.handlerType, f.model, opts.OriginalRequest)
	opts.Metadata = meta
	return req, opts
}

func fallbackProviderAllowList(entries []string) ProviderAllowList {
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
