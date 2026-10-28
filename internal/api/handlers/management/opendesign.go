package management

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

var openDesignCookiePattern = regexp.MustCompile(`(?i)__Secure-better-auth\.session_token\s*=\s*([^\s;'"\^]+)`)

func parseOpenDesignSession(input string) string {
	input = strings.TrimSpace(input)
	if m := openDesignCookiePattern.FindStringSubmatch(input); len(m) == 2 {
		return m[1]
	}
	if input == "" || strings.ContainsAny(input, " \t\r\n;'\"^:") || (strings.Contains(input, "=") && !strings.HasSuffix(input, "=")) {
		return ""
	}
	return input
}

func openDesignError(c *gin.Context, status int, code, message string) {
	c.JSON(status, gin.H{"error": message, "code": code})
}

func (h *Handler) openDesignAuth(c *gin.Context, index string) *coreauth.Auth {
	auth := h.authByIndex(strings.TrimSpace(index))
	if auth == nil {
		openDesignError(c, http.StatusNotFound, "auth_not_found", "Credential not found")
		return nil
	}
	if !util.IsOpenDesignBaseURL(authAttribute(auth, "base_url")) {
		openDesignError(c, http.StatusBadRequest, "not_opendesign_credential", "Credential is not an OpenDesign credential")
		return nil
	}
	return auth
}

func (h *Handler) openDesignEntry(auth *coreauth.Auth) *config.OpenAICompatibilityAPIKey {
	for i := range h.cfg.OpenAICompatibility {
		provider := &h.cfg.OpenAICompatibility[i]
		if !strings.EqualFold(strings.TrimSpace(provider.Name), strings.TrimSpace(authAttribute(auth, "compat_name"))) {
			continue
		}
		for j := range provider.APIKeyEntries {
			entry := &provider.APIKeyEntries[j]
			if entry.APIKey == authAttribute(auth, "api_key") {
				return entry
			}
		}
	}
	return nil
}

func (h *Handler) openDesignSave(c *gin.Context, auth *coreauth.Auth, token, workspace string) bool {
	h.mu.Lock()
	entry := h.openDesignEntry(auth)
	if entry == nil {
		h.mu.Unlock()
		openDesignError(c, http.StatusNotFound, "auth_not_found", "Credential not found")
		return false
	}
	oldToken, oldWorkspace := entry.OpenDesignSession, entry.OpenDesignWorkspaceID
	entry.OpenDesignSession, entry.OpenDesignWorkspaceID = token, workspace
	if err := config.SaveConfigPreserveComments(h.configFilePath, h.cfg, c.GetBool(ConfigV8ContextKey)); err != nil {
		entry.OpenDesignSession, entry.OpenDesignWorkspaceID = oldToken, oldWorkspace
		h.mu.Unlock()
		openDesignError(c, http.StatusInternalServerError, "config_save_failed", "Could not save configuration")
		return false
	}
	snapshot := h.reloadSnapshotConfigLocked()
	h.mu.Unlock()
	h.reloadConfigAfterManagementSaveAsync(c.Request.Context(), snapshot)
	return true
}

func (h *Handler) openDesignSettings(auth *coreauth.Auth) (string, string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if entry := h.openDesignEntry(auth); entry != nil {
		return entry.OpenDesignSession, entry.OpenDesignWorkspaceID
	}
	return "", ""
}

func openDesignExpired(err error) bool {
	var upstream *helps.OpenDesignHTTPError
	return errors.As(err, &upstream) && upstream.Status == http.StatusUnauthorized
}

// OpenDesignConnect connects a credential to a dashboard session.
func (h *Handler) OpenDesignConnect(c *gin.Context) {
	var body struct {
		AuthIndex string `json:"auth_index"`
		Session   string `json:"session"`
	}
	if c.ShouldBindJSON(&body) != nil {
		openDesignError(c, http.StatusBadRequest, "invalid_session_input", "Invalid session input")
		return
	}
	token := parseOpenDesignSession(body.Session)
	if token == "" {
		openDesignError(c, http.StatusBadRequest, "invalid_session_input", "No session cookie found")
		return
	}
	auth := h.openDesignAuth(c, body.AuthIndex)
	if auth == nil {
		return
	}
	client := helps.NewProxyAwareHTTPClient(c.Request.Context(), h.cfg, auth, 0)
	session, err := helps.FetchOpenDesignSession(c.Request.Context(), client, token)
	if openDesignExpired(err) || (err == nil && session == nil) {
		openDesignError(c, http.StatusUnprocessableEntity, "opendesign_session_invalid", "OpenDesign session is invalid")
		return
	}
	if err != nil {
		openDesignError(c, http.StatusBadGateway, "opendesign_upstream_error", "OpenDesign session lookup failed")
		return
	}
	workspace, err := helps.FetchOpenDesignCurrentWorkspace(c.Request.Context(), client, token)
	if err != nil || workspace.ID == "" {
		openDesignError(c, http.StatusBadGateway, "opendesign_upstream_error", "OpenDesign workspace lookup failed")
		return
	}
	if session.RotatedToken != "" {
		token = session.RotatedToken
	}
	if !h.openDesignSave(c, auth, token, workspace.ID) {
		return
	}
	c.JSON(http.StatusOK, gin.H{"connected": true, "valid": true, "email": session.User.Email, "workspace_id": workspace.ID, "workspace_name": workspace.Name, "expires_at": session.Session.ExpiresAt.Format(time.RFC3339), "checked_at": time.Now().UTC().Format(time.RFC3339)})
}

// OpenDesignDisconnect disconnects a dashboard session.
func (h *Handler) OpenDesignDisconnect(c *gin.Context) {
	auth := h.openDesignAuth(c, c.Query("auth_index"))
	if auth == nil {
		return
	}
	if h.openDesignSave(c, auth, "", "") {
		c.JSON(http.StatusOK, gin.H{"connected": false})
	}
}

// OpenDesignStatus reports dashboard session status.
func (h *Handler) OpenDesignStatus(c *gin.Context) {
	auth := h.openDesignAuth(c, c.Query("auth_index"))
	if auth == nil {
		return
	}
	token, workspaceID := h.openDesignSettings(auth)
	if token == "" {
		c.JSON(http.StatusOK, gin.H{"connected": false, "valid": false})
		return
	}
	client := helps.NewProxyAwareHTTPClient(c.Request.Context(), h.cfg, auth, 0)
	session, err := helps.FetchOpenDesignSession(c.Request.Context(), client, token)
	response := gin.H{"connected": true, "valid": false, "workspace_id": workspaceID, "checked_at": time.Now().UTC().Format(time.RFC3339)}
	if err != nil || session == nil {
		response["error"] = "OpenDesign session check failed"
		c.JSON(http.StatusOK, response)
		return
	}
	response["valid"] = true
	response["email"] = session.User.Email
	response["expires_at"] = session.Session.ExpiresAt.Format(time.RFC3339)
	workspace, err := helps.FetchOpenDesignCurrentWorkspace(c.Request.Context(), client, token)
	if err == nil {
		response["workspace_name"] = workspace.Name
	}
	c.JSON(http.StatusOK, response)
}

// OpenDesignUsage reports dashboard usage.
func (h *Handler) OpenDesignUsage(c *gin.Context) {
	auth := h.openDesignAuth(c, c.Query("auth_index"))
	if auth == nil {
		return
	}
	token, workspace := h.openDesignSettings(auth)
	if token == "" {
		openDesignError(c, http.StatusConflict, "opendesign_not_connected", "OpenDesign is not connected")
		return
	}
	client := helps.NewProxyAwareHTTPClient(c.Request.Context(), h.cfg, auth, 0)
	session, err := helps.FetchOpenDesignSession(c.Request.Context(), client, token)
	if openDesignExpired(err) || (err == nil && session == nil) {
		openDesignError(c, http.StatusPreconditionFailed, "opendesign_session_expired", "OpenDesign session has expired")
		return
	}
	if err != nil {
		openDesignError(c, http.StatusBadGateway, "opendesign_upstream_error", "OpenDesign session lookup failed")
		return
	}
	usage, err := helps.FetchOpenDesignUsage(c.Request.Context(), client, token, workspace)
	if openDesignExpired(err) {
		openDesignError(c, http.StatusPreconditionFailed, "opendesign_session_expired", "OpenDesign session has expired")
		return
	}
	if err != nil {
		openDesignError(c, http.StatusBadGateway, "opendesign_upstream_error", "OpenDesign usage lookup failed")
		return
	}
	windows := make([]gin.H, 0, len(usage.Windows))
	for _, w := range usage.Windows {
		label := ""
		switch {
		case w.DurationSeconds == 18000:
			label = "5h"
		case w.DurationSeconds == 604800:
			label = "weekly"
		case w.DurationSeconds%3600 == 0 && w.DurationSeconds < 86400:
			label = strconv.FormatInt(w.DurationSeconds/3600, 10) + "h"
		default:
			label = fmt.Sprintf("%dd", w.DurationSeconds/86400)
		}
		windows = append(windows, gin.H{"label": label, "duration_seconds": w.DurationSeconds, "used_credits": w.UsedCredits, "limit_credits": w.LimitCredits, "remaining_credits": w.RemainingCredits, "window_start": w.WindowStart.Format(time.RFC3339), "resets_at": w.ResetsAt.Format(time.RFC3339)})
	}
	c.JSON(http.StatusOK, gin.H{"workspace_id": usage.WorkspaceID, "tier": usage.Tier, "eligible": usage.Eligible, "generated_at": usage.GeneratedAt.Format(time.RFC3339), "fetched_at": time.Now().UTC().Format(time.RFC3339), "windows": windows, "wallet": gin.H{"balance_usd": usage.Wallet.BalanceUSD}})
}

// StartOpenDesignKeepAlive starts the dashboard session refresh loop.
func (h *Handler) StartOpenDesignKeepAlive() {
	h.mu.Lock()
	if h.openDesignCancel != nil {
		h.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.openDesignCancel = cancel
	h.openDesignRefresh = make(chan struct{}, 1)
	refresh := h.openDesignRefresh
	h.mu.Unlock()
	go func() {
		timer := time.NewTimer(2 * time.Second)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-refresh:
			case <-timer.C:
			}
			h.refreshOpenDesignSessions(ctx)
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(6 * time.Hour)
		}
	}()
}

// StopOpenDesignKeepAlive stops the dashboard session refresh loop.
func (h *Handler) StopOpenDesignKeepAlive() {
	h.mu.Lock()
	cancel := h.openDesignCancel
	h.openDesignCancel = nil
	h.openDesignRefresh = nil
	h.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (h *Handler) refreshOpenDesignSessions(ctx context.Context) {
	h.mu.Lock()
	cfg := h.cfg.CloneForRuntime()
	h.mu.Unlock()
	if cfg == nil {
		return
	}
	seen := make(map[string]bool)
	for _, provider := range cfg.OpenAICompatibility {
		if !util.IsOpenDesignBaseURL(provider.BaseURL) {
			continue
		}
		for _, entry := range provider.APIKeyEntries {
			token := entry.OpenDesignSession
			if token == "" || seen[token] {
				continue
			}
			seen[token] = true
			auth := &coreauth.Auth{ProxyURL: entry.ProxyURL}
			client := helps.NewProxyAwareHTTPClient(ctx, cfg, auth, 15*time.Second)
			session, err := helps.FetchOpenDesignSession(ctx, client, token)
			invalid := openDesignExpired(err) || (err == nil && session == nil)
			h.mu.Lock()
			if h.openDesignInvalid == nil {
				h.openDesignInvalid = make(map[string]bool)
			}
			wasInvalid := h.openDesignInvalid[token]
			if invalid {
				h.openDesignInvalid[token] = true
			} else if err == nil {
				delete(h.openDesignInvalid, token)
			}
			h.mu.Unlock()
			if invalid && !wasInvalid {
				log.Warn("OpenDesign session is invalid")
			}
			if err != nil || session == nil || session.RotatedToken == "" {
				continue
			}
			h.mu.Lock()
			changed := false
			for i := range h.cfg.OpenAICompatibility {
				p := &h.cfg.OpenAICompatibility[i]
				if !util.IsOpenDesignBaseURL(p.BaseURL) {
					continue
				}
				for j := range p.APIKeyEntries {
					e := &p.APIKeyEntries[j]
					if e.OpenDesignSession == token {
						e.OpenDesignSession = session.RotatedToken
						changed = true
					}
				}
			}
			if changed {
				if errSave := config.SaveConfigPreserveComments(h.configFilePath, h.cfg); errSave != nil {
					for i := range h.cfg.OpenAICompatibility {
						p := &h.cfg.OpenAICompatibility[i]
						for j := range p.APIKeyEntries {
							e := &p.APIKeyEntries[j]
							if e.OpenDesignSession == session.RotatedToken {
								e.OpenDesignSession = token
							}
						}
					}
					log.Warn("Could not save rotated OpenDesign session")
				} else {
					snapshot := h.reloadSnapshotConfigLocked()
					h.mu.Unlock()
					h.reloadConfigAfterManagementSaveAsync(ctx, snapshot)
					continue
				}
			}
			h.mu.Unlock()
		}
	}
}
