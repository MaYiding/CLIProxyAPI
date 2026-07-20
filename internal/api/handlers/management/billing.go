package management

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/billing"
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
