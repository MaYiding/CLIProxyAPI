package management

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/billing"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

type billingKeySettings struct {
	KeyID        string  `json:"key_id"`
	KeyMask      string  `json:"key_mask"`
	KeyPreview   string  `json:"key_preview"`
	KeyTruncated bool    `json:"key_truncated"`
	Name         string  `json:"name"`
	Label        string  `json:"label,omitempty"`
	Limit        float64 `json:"limit"`
	Limited      bool    `json:"limited"`
	Blocked      bool    `json:"blocked"`
	Spent        string  `json:"spent"`
	Remaining    string  `json:"remaining"`
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
	Name  string  `json:"name"`
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

const (
	billingKeyNameMaxBytes    = 128
	billingKeyPreviewMaxRunes = 16
	billingKeyPreviewPrefix   = 8
	billingKeyPreviewSuffix   = 4
)

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

// GetBillingSettings returns editable billing configuration. Active client
// keys are represented by a stable identifier and a bounded real preview; the
// complete value is available only through RevealBillingKey.
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

// RevealBillingKey returns one complete active client API key to an already
// authenticated management session. The response is deliberately non-cacheable
// and the key never appears in the request URL, logs, usage reports, or ledger.
func (h *Handler) RevealBillingKey(c *gin.Context) {
	c.Header("Cache-Control", "no-store, private, max-age=0")
	c.Header("Pragma", "no-cache")
	c.Header("X-Content-Type-Options", "nosniff")

	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler not initialized"})
		return
	}
	keyID := strings.ToLower(strings.TrimSpace(c.Param("key_id")))
	if len(keyID) != 64 {
		c.JSON(http.StatusNotFound, gin.H{"error": "active client API key not found"})
		return
	}

	h.mu.Lock()
	if h.cfg == nil {
		h.mu.Unlock()
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "configuration unavailable"})
		return
	}
	var revealed string
	for _, rawKey := range h.cfg.APIKeys {
		candidate := strings.TrimSpace(rawKey)
		candidateID, _ := billing.IdentifyKey(candidate)
		if strings.EqualFold(candidateID, keyID) {
			revealed = candidate
			break
		}
	}
	h.mu.Unlock()

	if revealed == "" {
		c.JSON(http.StatusNotFound, gin.H{"error": "active client API key not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"key": revealed})
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
		name, errName := billingKeyName(keyUpdate)
		if errName != nil {
			h.mu.Unlock()
			c.JSON(http.StatusBadRequest, gin.H{"error": errName.Error()})
			return
		}
		if name != "" {
			labels[rawKey] = name
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
		keyPreview, keyTruncated := billingKeyPreview(rawKey)
		status := billing.LimitStatus{Allowed: true}
		if manager != nil {
			status = manager.LimitStatus(rawKey)
		}
		label := strings.TrimSpace(cfg.Billing.KeyLabels[rawKey])
		limit := cfg.Billing.KeyLimits[rawKey]
		response.Keys = append(response.Keys, billingKeySettings{
			KeyID:        keyID,
			KeyMask:      keyMask,
			KeyPreview:   keyPreview,
			KeyTruncated: keyTruncated,
			Name:         label,
			Label:        label,
			Limit:        limit,
			Limited:      limit > 0,
			Blocked:      status.Blocked,
			Spent:        status.Spent,
			Remaining:    status.Remaining,
		})
	}
	return response
}

func billingRawKeys(cfg *config.Config) []string {
	if cfg == nil {
		return nil
	}
	keys := make([]string, 0, len(cfg.APIKeys))
	seen := make(map[string]struct{})
	for _, rawKey := range cfg.APIKeys {
		rawKey = strings.TrimSpace(rawKey)
		if rawKey == "" {
			continue
		}
		if _, exists := seen[rawKey]; exists {
			continue
		}
		seen[rawKey] = struct{}{}
		keys = append(keys, rawKey)
	}
	return keys
}

func billingKeyName(update billingKeySettingsUpdate) (string, error) {
	name := strings.TrimSpace(update.Name)
	legacyLabel := strings.TrimSpace(update.Label)
	if name != "" && legacyLabel != "" && name != legacyLabel {
		return "", errors.New("key name and legacy label must match when both are provided")
	}
	if name == "" {
		name = legacyLabel
	}
	if !utf8.ValidString(name) {
		return "", errors.New("key names must be valid UTF-8")
	}
	if len(name) > billingKeyNameMaxBytes {
		return "", fmt.Errorf("key names must not exceed %d bytes", billingKeyNameMaxBytes)
	}
	for _, character := range name {
		if unicode.IsControl(character) {
			return "", errors.New("key names must not contain control characters")
		}
	}
	return name, nil
}

func billingKeyPreview(rawKey string) (string, bool) {
	runes := []rune(strings.TrimSpace(rawKey))
	if len(runes) <= billingKeyPreviewMaxRunes {
		return string(runes), false
	}
	return string(runes[:billingKeyPreviewPrefix]) + "…" + string(runes[len(runes)-billingKeyPreviewSuffix:]), true
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
