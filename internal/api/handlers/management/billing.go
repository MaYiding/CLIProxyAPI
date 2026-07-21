package management

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/billing"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

type billingKeySettings struct {
	KeyID     string  `json:"key_id"`
	KeyMask   string  `json:"key_mask"`
	Label     string  `json:"label"`
	Limit     float64 `json:"limit"`
	Limited   bool    `json:"limited"`
	Blocked   bool    `json:"blocked"`
	Spent     string  `json:"spent"`
	Remaining string  `json:"remaining"`
}

type billingSettingsResponse struct {
	Enabled                bool                  `json:"enabled"`
	Currency               string                `json:"currency"`
	StorePath              string                `json:"store_path"`
	SyncOnWrite            bool                  `json:"sync_on_write"`
	DefaultPricePerMillion float64               `json:"default_price_per_million"`
	Prices                 []config.BillingPrice `json:"prices"`
	Keys                   []billingKeySettings  `json:"keys"`
}

type billingKeySettingsUpdate struct {
	KeyID string  `json:"key_id"`
	Label string  `json:"label"`
	Limit float64 `json:"limit"`
}

type billingSettingsUpdate struct {
	Enabled                *bool                       `json:"enabled"`
	SyncOnWrite            *bool                       `json:"sync_on_write"`
	DefaultPricePerMillion *float64                    `json:"default_price_per_million"`
	Prices                 *[]config.BillingPrice      `json:"prices"`
	Keys                   *[]billingKeySettingsUpdate `json:"keys"`
}

// GetBillingUsage returns persistent per-key usage aggregates and request details.
func (h *Handler) GetBillingUsage(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler not initialized"})
		return
	}
	query, errQuery := parseBillingQuery(c)
	if errQuery != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errQuery.Error()})
		return
	}

	h.mu.Lock()
	manager := h.billingManager
	h.mu.Unlock()
	if manager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "billing manager unavailable"})
		return
	}

	c.JSON(http.StatusOK, manager.Snapshot(query))
}

// GetBillingSettings returns editable billing configuration without exposing
// raw client API keys.
func (h *Handler) GetBillingSettings(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler not initialized"})
		return
	}
	h.mu.Lock()
	if h.cfg == nil {
		h.mu.Unlock()
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "configuration unavailable"})
		return
	}
	cfg := h.cfg.CloneForRuntime()
	manager := h.billingManager
	h.mu.Unlock()
	c.JSON(http.StatusOK, buildBillingSettingsResponse(cfg, manager))
}

// PutBillingSettings validates, persists, and immediately applies editable
// billing settings. Store path and currency remain YAML-only operational fields.
func (h *Handler) PutBillingSettings(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler not initialized"})
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20)
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	var update billingSettingsUpdate
	if errDecode := decoder.Decode(&update); errDecode != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid billing settings", "message": errDecode.Error()})
		return
	}
	if errTrailing := decoder.Decode(&struct{}{}); errTrailing != io.EOF {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid billing settings", "message": "request body must contain one JSON object"})
		return
	}
	if update.Enabled == nil || update.SyncOnWrite == nil || update.DefaultPricePerMillion == nil || update.Prices == nil || update.Keys == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "enabled, sync_on_write, default_price_per_million, prices, and keys are required"})
		return
	}

	h.mu.Lock()
	if h.cfg == nil {
		h.mu.Unlock()
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "configuration unavailable"})
		return
	}
	rawKeys := billingRawKeys(h.cfg)
	rawByID := make(map[string]string, len(rawKeys))
	for _, rawKey := range rawKeys {
		keyID, _ := billing.IdentifyKey(rawKey)
		rawByID[keyID] = rawKey
	}
	if len(*update.Keys) != len(rawByID) {
		h.mu.Unlock()
		c.JSON(http.StatusConflict, gin.H{"error": "client API keys changed; reload billing settings and try again"})
		return
	}

	labels := make(map[string]string)
	limits := make(map[string]float64)
	seen := make(map[string]struct{}, len(*update.Keys))
	for _, keyUpdate := range *update.Keys {
		keyID := strings.ToLower(strings.TrimSpace(keyUpdate.KeyID))
		rawKey, exists := rawByID[keyID]
		if !exists {
			h.mu.Unlock()
			c.JSON(http.StatusBadRequest, gin.H{"error": "unknown client API key identifier"})
			return
		}
		if _, duplicate := seen[keyID]; duplicate {
			h.mu.Unlock()
			c.JSON(http.StatusBadRequest, gin.H{"error": "duplicate client API key identifier"})
			return
		}
		seen[keyID] = struct{}{}
		label := strings.TrimSpace(keyUpdate.Label)
		if len(label) > 128 {
			h.mu.Unlock()
			c.JSON(http.StatusBadRequest, gin.H{"error": "key labels must not exceed 128 bytes"})
			return
		}
		if label != "" {
			labels[rawKey] = label
		}
		if keyUpdate.Limit != 0 {
			limits[rawKey] = keyUpdate.Limit
		}
	}

	candidate := h.cfg.Billing
	candidate.Enabled = *update.Enabled
	candidate.SyncOnWrite = *update.SyncOnWrite
	defaultPrice := *update.DefaultPricePerMillion
	candidate.DefaultPricePerMillion = &defaultPrice
	candidate.Prices = append([]config.BillingPrice(nil), (*update.Prices)...)
	candidate.KeyLabels = labels
	candidate.KeyLimits = limits
	if errValidate := billing.ValidateConfig(candidate); errValidate != nil {
		h.mu.Unlock()
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid billing settings", "message": errValidate.Error()})
		return
	}
	previousBilling := h.cfg.Billing
	h.cfg.Billing = candidate
	snapshot, saved := h.saveConfigAndSnapshotLocked(c)
	manager := h.billingManager
	if !saved {
		h.cfg.Billing = previousBilling
		h.mu.Unlock()
		return
	}
	if manager != nil {
		if errConfigure := manager.ConfigureForKeys(snapshot.cfg.Billing, snapshot.cfg.APIKeys, snapshot.cfg.AuthDir, h.configFilePath); errConfigure != nil {
			h.cfg.Billing = previousBilling
			errRollback := config.SaveConfigPreserveComments(h.configFilePath, h.cfg)
			h.mu.Unlock()
			if errRollback != nil {
				c.JSON(http.StatusInternalServerError, gin.H{
					"error":   "billing settings could not be applied and the saved configuration could not be rolled back",
					"message": errors.Join(errConfigure, errRollback).Error(),
				})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "billing settings could not be applied; changes were rolled back", "message": errConfigure.Error()})
			return
		}
	}
	h.mu.Unlock()
	h.reloadConfigAfterManagementSave(c.Request.Context(), snapshot)
	c.JSON(http.StatusOK, buildBillingSettingsResponse(snapshot.cfg, manager))
}

func buildBillingSettingsResponse(cfg *config.Config, manager *billing.Manager) billingSettingsResponse {
	response := billingSettingsResponse{Currency: "USD", Prices: []config.BillingPrice{}, Keys: []billingKeySettings{}}
	if cfg == nil {
		return response
	}
	response.Enabled = cfg.Billing.Enabled
	response.Currency = strings.ToUpper(strings.TrimSpace(cfg.Billing.Currency))
	if response.Currency == "" {
		response.Currency = "USD"
	}
	response.StorePath = cfg.Billing.StorePath
	response.SyncOnWrite = cfg.Billing.SyncOnWrite
	response.DefaultPricePerMillion = billing.EffectiveDefaultPricePerMillion(cfg.Billing)
	response.Prices = append(response.Prices, cfg.Billing.Prices...)
	for _, rawKey := range billingRawKeys(cfg) {
		keyID, keyMask := billing.IdentifyKey(rawKey)
		status := billing.LimitStatus{Allowed: true}
		if manager != nil {
			status = manager.LimitStatus(rawKey)
		}
		label := strings.TrimSpace(cfg.Billing.KeyLabels[rawKey])
		limit := cfg.Billing.KeyLimits[rawKey]
		response.Keys = append(response.Keys, billingKeySettings{
			KeyID:     keyID,
			KeyMask:   keyMask,
			Label:     label,
			Limit:     limit,
			Limited:   limit > 0,
			Blocked:   status.Blocked,
			Spent:     status.Spent,
			Remaining: status.Remaining,
		})
	}
	return response
}

func billingRawKeys(cfg *config.Config) []string {
	if cfg == nil {
		return nil
	}
	keys := make([]string, 0, len(cfg.APIKeys)+len(cfg.Billing.KeyLabels)+len(cfg.Billing.KeyLimits))
	seen := make(map[string]struct{})
	appendKey := func(rawKey string) {
		rawKey = strings.TrimSpace(rawKey)
		if rawKey == "" {
			return
		}
		if _, exists := seen[rawKey]; exists {
			return
		}
		seen[rawKey] = struct{}{}
		keys = append(keys, rawKey)
	}
	for _, rawKey := range cfg.APIKeys {
		appendKey(rawKey)
	}
	orphans := make([]string, 0, len(cfg.Billing.KeyLabels)+len(cfg.Billing.KeyLimits))
	for rawKey := range cfg.Billing.KeyLabels {
		if _, exists := seen[strings.TrimSpace(rawKey)]; !exists {
			orphans = append(orphans, rawKey)
		}
	}
	for rawKey := range cfg.Billing.KeyLimits {
		if _, exists := seen[strings.TrimSpace(rawKey)]; !exists {
			orphans = append(orphans, rawKey)
		}
	}
	sort.Strings(orphans)
	for _, rawKey := range orphans {
		appendKey(rawKey)
	}
	return keys
}

func parseBillingQuery(c *gin.Context) (billing.Query, error) {
	query := billing.Query{
		KeyID:    c.Query("key_id"),
		Provider: c.Query("provider"),
		Model:    c.Query("model"),
	}
	var err error
	if query.From, err = parseBillingTime(c.Query("from")); err != nil {
		return billing.Query{}, fmt.Errorf("invalid from: %w", err)
	}
	if query.To, err = parseBillingTime(c.Query("to")); err != nil {
		return billing.Query{}, fmt.Errorf("invalid to: %w", err)
	}
	if !query.From.IsZero() && !query.To.IsZero() && query.From.After(query.To) {
		return billing.Query{}, fmt.Errorf("from must not be after to")
	}
	if query.Limit, err = parseNonNegativeInt(c.Query("limit"), 100); err != nil {
		return billing.Query{}, fmt.Errorf("invalid limit: %w", err)
	}
	if query.Limit == 0 {
		return billing.Query{}, fmt.Errorf("limit must be positive")
	}
	if query.Offset, err = parseNonNegativeInt(c.Query("offset"), 0); err != nil {
		return billing.Query{}, fmt.Errorf("invalid offset: %w", err)
	}
	return query, nil
}

func parseBillingTime(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, nil
	}
	if unixSeconds, errUnix := strconv.ParseInt(value, 10, 64); errUnix == nil {
		return time.Unix(unixSeconds, 0).UTC(), nil
	}
	parsed, errParse := time.Parse(time.RFC3339, value)
	if errParse != nil {
		return time.Time{}, fmt.Errorf("use RFC3339 or Unix seconds")
	}
	return parsed.UTC(), nil
}

func parseNonNegativeInt(value string, fallback int) (int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback, nil
	}
	parsed, errParse := strconv.Atoi(value)
	if errParse != nil || parsed < 0 {
		return 0, fmt.Errorf("must be a non-negative integer")
	}
	return parsed, nil
}
