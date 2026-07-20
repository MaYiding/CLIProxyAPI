// Package billing provides persistent per-client API key usage metering.
package billing

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	internallogging "github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

const (
	defaultCurrency = "USD"
	defaultFileName = "billing.jsonl"
	modeAuto        = "auto"
	modeIncluded    = "included"
	modeAdditional  = "additional"
)

// TokenUsage stores both provider-reported counters and normalized billable counters.
type TokenUsage struct {
	InputTokens          int64 `json:"input_tokens"`
	OutputTokens         int64 `json:"output_tokens"`
	ReasoningTokens      int64 `json:"reasoning_tokens"`
	CachedTokens         int64 `json:"cached_tokens"`
	CacheReadTokens      int64 `json:"cache_read_tokens"`
	CacheCreationTokens  int64 `json:"cache_creation_tokens"`
	TotalTokens          int64 `json:"total_tokens"`
	BillableInputTokens  int64 `json:"billable_input_tokens"`
	BillableOutputTokens int64 `json:"billable_output_tokens"`
}

// PricingSnapshot freezes the rule and rates used to price an event.
type PricingSnapshot struct {
	Priced                  bool    `json:"priced"`
	Rule                    string  `json:"rule,omitempty"`
	InputPerMillion         float64 `json:"input_per_million,omitempty"`
	OutputPerMillion        float64 `json:"output_per_million,omitempty"`
	ReasoningPerMillion     float64 `json:"reasoning_per_million,omitempty"`
	CacheReadPerMillion     float64 `json:"cache_read_per_million,omitempty"`
	CacheCreationPerMillion float64 `json:"cache_creation_per_million,omitempty"`
	InputCacheMode          string  `json:"input_cache_mode"`
	ReasoningMode           string  `json:"reasoning_mode"`
}

// CostBreakdown stores costs in billionths of the configured currency unit.
type CostBreakdown struct {
	Currency           string `json:"currency"`
	InputNanos         int64  `json:"input_nanos"`
	OutputNanos        int64  `json:"output_nanos"`
	ReasoningNanos     int64  `json:"reasoning_nanos"`
	CacheReadNanos     int64  `json:"cache_read_nanos"`
	CacheCreationNanos int64  `json:"cache_creation_nanos"`
	TotalNanos         int64  `json:"total_nanos"`
	Total              string `json:"total"`
}

// Event is one immutable request-level billing ledger record.
type Event struct {
	ID              string          `json:"id"`
	Timestamp       time.Time       `json:"timestamp"`
	RequestID       string          `json:"request_id,omitempty"`
	KeyID           string          `json:"key_id"`
	KeyLabel        string          `json:"key_label"`
	KeyMask         string          `json:"key_mask"`
	Provider        string          `json:"provider"`
	ExecutorType    string          `json:"executor_type,omitempty"`
	Model           string          `json:"model"`
	Alias           string          `json:"alias,omitempty"`
	Endpoint        string          `json:"endpoint,omitempty"`
	AuthID          string          `json:"auth_id,omitempty"`
	AuthIndex       string          `json:"auth_index,omitempty"`
	AuthType        string          `json:"auth_type,omitempty"`
	Source          string          `json:"source,omitempty"`
	SourceID        string          `json:"source_id,omitempty"`
	ReasoningEffort string          `json:"reasoning_effort,omitempty"`
	ServiceTier     string          `json:"service_tier,omitempty"`
	LatencyMS       int64           `json:"latency_ms"`
	TTFTMS          int64           `json:"ttft_ms"`
	Failed          bool            `json:"failed"`
	StatusCode      int             `json:"status_code"`
	Tokens          TokenUsage      `json:"tokens"`
	Pricing         PricingSnapshot `json:"pricing"`
	Cost            CostBreakdown   `json:"cost"`
}

// Query filters and paginates billing events. Aggregates always cover all matches.
type Query struct {
	From     time.Time
	To       time.Time
	KeyID    string
	Provider string
	Model    string
	Limit    int
	Offset   int
}

// Totals contains request, token, and cost aggregates.
type Totals struct {
	Requests         int64         `json:"requests"`
	Success          int64         `json:"success"`
	Failed           int64         `json:"failed"`
	PricedRequests   int64         `json:"priced_requests"`
	UnpricedRequests int64         `json:"unpriced_requests"`
	Tokens           TokenUsage    `json:"tokens"`
	Cost             CostBreakdown `json:"cost"`
}

// KeySummary aggregates matched usage for one client API key.
type KeySummary struct {
	KeyID    string `json:"key_id"`
	KeyLabel string `json:"key_label"`
	KeyMask  string `json:"key_mask"`
	Totals
}

// ModelSummary aggregates matched usage for one provider and model.
type ModelSummary struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Totals
}

// Report is the management API representation of a billing query.
type Report struct {
	Enabled       bool           `json:"enabled"`
	Currency      string         `json:"currency"`
	StorePath     string         `json:"store_path,omitempty"`
	SyncOnWrite   bool           `json:"sync_on_write"`
	LedgerEvents  int            `json:"ledger_events"`
	MatchedEvents int            `json:"matched_events"`
	Limit         int            `json:"limit"`
	Offset        int            `json:"offset"`
	Totals        Totals         `json:"totals"`
	ByKey         []KeySummary   `json:"by_key"`
	ByModel       []ModelSummary `json:"by_model"`
	Events        []Event        `json:"events"`
}

type compiledPrice struct {
	rule          config.BillingPrice
	name          string
	provider      string
	model         string
	cacheMode     string
	reasoningMode string
}

// Manager writes usage records to an append-only ledger and serves snapshots.
type Manager struct {
	mu          sync.RWMutex
	enabled     bool
	currency    string
	storePath   string
	syncOnWrite bool
	file        *os.File
	events      []Event
	prices      []compiledPrice
	keyLabels   map[string]string
	keyIDLabels map[string]string
}

var defaultManager = NewManager()

func init() {
	coreusage.RegisterNamedPlugin("builtin:billing", defaultManager)
}

// NewManager creates a disabled billing manager.
func NewManager() *Manager {
	return &Manager{currency: defaultCurrency}
}

// DefaultManager returns the process-wide billing manager registered with usage reporting.
func DefaultManager() *Manager { return defaultManager }

// Configure atomically replaces the billing configuration and reloads the ledger.
func (m *Manager) Configure(cfg config.BillingConfig, authDir, configFilePath string) error {
	if m == nil {
		return errors.New("billing manager is nil")
	}

	currency := strings.ToUpper(strings.TrimSpace(cfg.Currency))
	if currency == "" {
		currency = defaultCurrency
	}
	prices, errPrices := compilePrices(cfg.Prices)
	if errPrices != nil {
		return errPrices
	}
	labels := normalizeKeyLabels(cfg.KeyLabels)
	keyIDLabels := labelsByKeyID(labels)

	if !cfg.Enabled {
		m.mu.Lock()
		defer m.mu.Unlock()
		errClose := m.closeLocked()
		m.enabled = false
		m.currency = currency
		m.storePath = ""
		m.syncOnWrite = false
		m.events = nil
		m.prices = prices
		m.keyLabels = labels
		m.keyIDLabels = keyIDLabels
		if errClose != nil {
			return fmt.Errorf("billing: close ledger: %w", errClose)
		}
		return nil
	}

	storePath, errPath := resolveStorePath(cfg.StorePath, authDir, configFilePath)
	if errPath != nil {
		return errPath
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.enabled && m.file != nil && m.currency == currency && m.storePath == storePath &&
		m.syncOnWrite == cfg.SyncOnWrite && reflect.DeepEqual(m.prices, prices) && reflect.DeepEqual(m.keyLabels, labels) {
		return nil
	}
	if errMkdir := os.MkdirAll(filepath.Dir(storePath), 0o700); errMkdir != nil {
		return fmt.Errorf("billing: create ledger directory: %w", errMkdir)
	}

	file, errOpen := os.OpenFile(storePath, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if errOpen != nil {
		return fmt.Errorf("billing: open ledger: %w", errOpen)
	}
	events, errLoad := loadEvents(file)
	if errLoad != nil {
		if errClose := file.Close(); errClose != nil {
			log.WithError(errClose).Warn("billing: failed to close unusable ledger")
		}
		return errLoad
	}
	if errCurrency := normalizeLedgerCurrency(events, currency); errCurrency != nil {
		if errClose := file.Close(); errClose != nil {
			log.WithError(errClose).Warn("billing: failed to close ledger with mismatched currency")
		}
		return errCurrency
	}

	if errClose := m.closeLocked(); errClose != nil {
		log.WithError(errClose).Warn("billing: failed to close previous ledger")
	}
	m.enabled = true
	m.currency = currency
	m.storePath = storePath
	m.syncOnWrite = cfg.SyncOnWrite
	m.file = file
	m.events = events
	m.prices = prices
	m.keyLabels = labels
	m.keyIDLabels = keyIDLabels
	return nil
}

// HandleUsage implements usage.Plugin.
func (m *Manager) HandleUsage(ctx context.Context, record coreusage.Record) {
	if m == nil {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.enabled || m.file == nil {
		return
	}

	event := m.buildEventLocked(ctx, record)
	payload, errMarshal := json.Marshal(event)
	if errMarshal != nil {
		log.WithError(errMarshal).Error("billing: failed to encode usage event")
		return
	}
	payload = append(payload, '\n')
	if errWrite := writeLedgerLine(m.file, payload); errWrite != nil {
		log.WithError(errWrite).Error("billing: failed to append usage event")
		return
	}
	if m.syncOnWrite {
		if errSync := m.file.Sync(); errSync != nil {
			log.WithError(errSync).Error("billing: failed to sync usage event")
		}
	}
	m.events = append(m.events, event)
}

// Snapshot returns filtered aggregates plus a reverse-chronological event page.
func (m *Manager) Snapshot(query Query) Report {
	if m == nil {
		return Report{Currency: defaultCurrency, ByKey: []KeySummary{}, ByModel: []ModelSummary{}, Events: []Event{}}
	}

	m.mu.RLock()
	enabled := m.enabled
	currency := m.currency
	storePath := m.storePath
	syncOnWrite := m.syncOnWrite
	events := append([]Event(nil), m.events...)
	keyIDLabels := m.keyIDLabels
	m.mu.RUnlock()

	if currency == "" {
		currency = defaultCurrency
	}
	query = normalizeQuery(query)
	report := Report{
		Enabled:      enabled,
		Currency:     currency,
		StorePath:    storePath,
		SyncOnWrite:  syncOnWrite,
		LedgerEvents: len(events),
		Limit:        query.Limit,
		Offset:       query.Offset,
		ByKey:        []KeySummary{},
		ByModel:      []ModelSummary{},
		Events:       []Event{},
	}
	keyTotals := make(map[string]*KeySummary)
	modelTotals := make(map[string]*ModelSummary)
	matched := make([]Event, 0, len(events))

	for i := range events {
		event := events[i]
		if label := keyIDLabels[event.KeyID]; label != "" {
			event.KeyLabel = label
		}
		if !matchesQuery(event, query) {
			continue
		}
		matched = append(matched, event)
		addEventToTotals(&report.Totals, event, currency)

		keySummary := keyTotals[event.KeyID]
		if keySummary == nil {
			keySummary = &KeySummary{KeyID: event.KeyID, KeyLabel: event.KeyLabel, KeyMask: event.KeyMask}
			keyTotals[event.KeyID] = keySummary
		}
		addEventToTotals(&keySummary.Totals, event, currency)

		modelKey := event.Provider + "\x00" + event.Model
		modelSummary := modelTotals[modelKey]
		if modelSummary == nil {
			modelSummary = &ModelSummary{Provider: event.Provider, Model: event.Model}
			modelTotals[modelKey] = modelSummary
		}
		addEventToTotals(&modelSummary.Totals, event, currency)
	}

	report.MatchedEvents = len(matched)
	finalizeTotals(&report.Totals, currency)
	for _, summary := range keyTotals {
		finalizeTotals(&summary.Totals, currency)
		report.ByKey = append(report.ByKey, *summary)
	}
	for _, summary := range modelTotals {
		finalizeTotals(&summary.Totals, currency)
		report.ByModel = append(report.ByModel, *summary)
	}
	sort.Slice(report.ByKey, func(i, j int) bool {
		if report.ByKey[i].Cost.TotalNanos == report.ByKey[j].Cost.TotalNanos {
			return report.ByKey[i].KeyLabel < report.ByKey[j].KeyLabel
		}
		return report.ByKey[i].Cost.TotalNanos > report.ByKey[j].Cost.TotalNanos
	})
	sort.Slice(report.ByModel, func(i, j int) bool {
		if report.ByModel[i].Cost.TotalNanos == report.ByModel[j].Cost.TotalNanos {
			if report.ByModel[i].Provider == report.ByModel[j].Provider {
				return report.ByModel[i].Model < report.ByModel[j].Model
			}
			return report.ByModel[i].Provider < report.ByModel[j].Provider
		}
		return report.ByModel[i].Cost.TotalNanos > report.ByModel[j].Cost.TotalNanos
	})
	sort.SliceStable(matched, func(i, j int) bool {
		return matched[i].Timestamp.After(matched[j].Timestamp)
	})
	start := query.Offset
	if start > len(matched) {
		start = len(matched)
	}
	end := start + query.Limit
	if end > len(matched) {
		end = len(matched)
	}
	report.Events = append(report.Events, matched[start:end]...)
	return report
}

// Flush asks the operating system to persist the current ledger contents.
func (m *Manager) Flush() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.file == nil {
		return nil
	}
	if errSync := m.file.Sync(); errSync != nil {
		return fmt.Errorf("billing: sync ledger: %w", errSync)
	}
	return nil
}

// Close closes the active ledger and disables recording.
func (m *Manager) Close() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	errClose := m.closeLocked()
	m.enabled = false
	return errClose
}

func (m *Manager) closeLocked() error {
	if m.file == nil {
		return nil
	}
	errSync := m.file.Sync()
	errClose := m.file.Close()
	m.file = nil
	return errors.Join(errSync, errClose)
}

func (m *Manager) buildEventLocked(ctx context.Context, record coreusage.Record) Event {
	timestamp := record.RequestedAt
	if timestamp.IsZero() {
		timestamp = time.Now()
	}
	provider := normalizedValue(record.Provider, "unknown")
	model := normalizedValue(record.Model, "unknown")
	keyID, keyMask := identifyKey(record.APIKey)
	keyLabel := keyMask
	if label := strings.TrimSpace(m.keyLabels[strings.TrimSpace(record.APIKey)]); label != "" {
		keyLabel = label
	}

	price := matchPrice(m.prices, provider, model)
	pricing, tokens, cost := priceRecord(record.Detail, price, m.currency)
	statusCode := record.Fail.StatusCode
	if statusCode <= 0 {
		statusCode = internallogging.GetResponseStatus(ctx)
	}
	failed := record.Failed || statusCode >= http.StatusBadRequest
	if statusCode <= 0 {
		if failed {
			statusCode = http.StatusInternalServerError
		} else {
			statusCode = http.StatusOK
		}
	}
	serviceTier := strings.TrimSpace(record.ResponseServiceTier)
	if serviceTier == "" {
		serviceTier = strings.TrimSpace(record.RequestServiceTier)
	}
	if serviceTier == "" {
		serviceTier = strings.TrimSpace(record.ServiceTier)
	}
	source, sourceID := billingSource(record.Source, record.AuthType)

	return Event{
		ID:              uuid.NewString(),
		Timestamp:       timestamp.UTC(),
		RequestID:       strings.TrimSpace(internallogging.GetRequestID(ctx)),
		KeyID:           keyID,
		KeyLabel:        keyLabel,
		KeyMask:         keyMask,
		Provider:        provider,
		ExecutorType:    strings.TrimSpace(record.ExecutorType),
		Model:           model,
		Alias:           strings.TrimSpace(record.Alias),
		Endpoint:        strings.TrimSpace(internallogging.GetEndpoint(ctx)),
		AuthID:          strings.TrimSpace(record.AuthID),
		AuthIndex:       strings.TrimSpace(record.AuthIndex),
		AuthType:        strings.TrimSpace(record.AuthType),
		Source:          source,
		SourceID:        sourceID,
		ReasoningEffort: strings.TrimSpace(record.ReasoningEffort),
		ServiceTier:     serviceTier,
		LatencyMS:       record.Latency.Milliseconds(),
		TTFTMS:          record.TTFT.Milliseconds(),
		Failed:          failed,
		StatusCode:      statusCode,
		Tokens:          tokens,
		Pricing:         pricing,
		Cost:            cost,
	}
}

func compilePrices(rules []config.BillingPrice) ([]compiledPrice, error) {
	compiled := make([]compiledPrice, 0, len(rules))
	for i := range rules {
		rule := rules[i]
		if errRate := validateRates(rule); errRate != nil {
			return nil, fmt.Errorf("billing: price rule %d: %w", i, errRate)
		}
		cacheMode, errCacheMode := normalizeMode(rule.InputCacheMode)
		if errCacheMode != nil {
			return nil, fmt.Errorf("billing: price rule %d input-cache-mode: %w", i, errCacheMode)
		}
		reasoningMode, errReasoningMode := normalizeMode(rule.ReasoningMode)
		if errReasoningMode != nil {
			return nil, fmt.Errorf("billing: price rule %d reasoning-mode: %w", i, errReasoningMode)
		}
		provider := strings.ToLower(strings.TrimSpace(rule.Provider))
		if provider == "" {
			provider = "*"
		}
		model := strings.ToLower(strings.TrimSpace(rule.Model))
		if model == "" {
			model = "*"
		}
		name := strings.TrimSpace(rule.Name)
		if name == "" {
			name = provider + "/" + model
		}
		compiled = append(compiled, compiledPrice{
			rule:          rule,
			name:          name,
			provider:      provider,
			model:         model,
			cacheMode:     cacheMode,
			reasoningMode: reasoningMode,
		})
	}
	return compiled, nil
}

func validateRates(rule config.BillingPrice) error {
	rates := []float64{
		rule.InputPerMillion,
		rule.OutputPerMillion,
		rule.ReasoningPerMillion,
		rule.CacheReadPerMillion,
		rule.CacheCreationPerMillion,
	}
	for _, rate := range rates {
		if math.IsNaN(rate) || math.IsInf(rate, 0) || rate < 0 {
			return errors.New("token prices must be finite and non-negative")
		}
	}
	return nil
}

func normalizeMode(value string) (string, error) {
	mode := strings.ToLower(strings.TrimSpace(value))
	if mode == "" {
		return modeAuto, nil
	}
	switch mode {
	case modeAuto, modeIncluded, modeAdditional:
		return mode, nil
	default:
		return "", fmt.Errorf("must be one of %q, %q, or %q", modeAuto, modeIncluded, modeAdditional)
	}
}

func matchPrice(prices []compiledPrice, provider, model string) *compiledPrice {
	provider = strings.ToLower(strings.TrimSpace(provider))
	model = strings.ToLower(strings.TrimSpace(model))
	for i := range prices {
		if wildcardMatch(prices[i].provider, provider) && wildcardMatch(prices[i].model, model) {
			return &prices[i]
		}
	}
	return nil
}

func wildcardMatch(pattern, value string) bool {
	pattern = strings.ToLower(pattern)
	value = strings.ToLower(value)
	dp := make([]bool, len(value)+1)
	dp[0] = true
	for i := 0; i < len(pattern); i++ {
		next := make([]bool, len(value)+1)
		switch pattern[i] {
		case '*':
			next[0] = dp[0]
			for j := 1; j <= len(value); j++ {
				next[j] = dp[j] || next[j-1]
			}
		case '?':
			for j := 1; j <= len(value); j++ {
				next[j] = dp[j-1]
			}
		default:
			for j := 1; j <= len(value); j++ {
				next[j] = dp[j-1] && pattern[i] == value[j-1]
			}
		}
		dp = next
	}
	return dp[len(value)]
}

func priceRecord(detail coreusage.Detail, price *compiledPrice, currency string) (PricingSnapshot, TokenUsage, CostBreakdown) {
	cacheRead := nonNegative(detail.CacheReadTokens)
	if cacheRead == 0 {
		cacheRead = nonNegative(detail.CachedTokens)
	}
	cacheCreation := nonNegative(detail.CacheCreationTokens)
	input := nonNegative(detail.InputTokens)
	output := nonNegative(detail.OutputTokens)
	reasoning := nonNegative(detail.ReasoningTokens)
	total := nonNegative(detail.TotalTokens)
	if total == 0 {
		total = saturatingAdd(saturatingAdd(input, output), reasoning)
	}

	cacheMode := modeAuto
	reasoningMode := modeAuto
	if price != nil {
		cacheMode = price.cacheMode
		reasoningMode = price.reasoningMode
	}
	resolvedCacheMode, resolvedReasoningMode := resolveCounterModes(detail, cacheMode, reasoningMode)
	billableInput := input
	if resolvedCacheMode == modeIncluded {
		billableInput = subtractFloorZero(billableInput, saturatingAdd(cacheRead, cacheCreation))
	}
	billableOutput := output
	if resolvedReasoningMode == modeIncluded {
		billableOutput = subtractFloorZero(billableOutput, reasoning)
	}

	tokens := TokenUsage{
		InputTokens:          input,
		OutputTokens:         output,
		ReasoningTokens:      reasoning,
		CachedTokens:         nonNegative(detail.CachedTokens),
		CacheReadTokens:      cacheRead,
		CacheCreationTokens:  cacheCreation,
		TotalTokens:          total,
		BillableInputTokens:  billableInput,
		BillableOutputTokens: billableOutput,
	}
	pricing := PricingSnapshot{
		InputCacheMode: resolvedCacheMode,
		ReasoningMode:  resolvedReasoningMode,
	}
	cost := CostBreakdown{Currency: currency}
	if price == nil {
		cost.Total = formatNanos(0)
		return pricing, tokens, cost
	}

	pricing.Priced = true
	pricing.Rule = price.name
	pricing.InputPerMillion = price.rule.InputPerMillion
	pricing.OutputPerMillion = price.rule.OutputPerMillion
	pricing.ReasoningPerMillion = price.rule.ReasoningPerMillion
	pricing.CacheReadPerMillion = price.rule.CacheReadPerMillion
	pricing.CacheCreationPerMillion = price.rule.CacheCreationPerMillion
	cost.InputNanos = tokenCostNanos(billableInput, price.rule.InputPerMillion)
	cost.OutputNanos = tokenCostNanos(billableOutput, price.rule.OutputPerMillion)
	cost.ReasoningNanos = tokenCostNanos(reasoning, price.rule.ReasoningPerMillion)
	cost.CacheReadNanos = tokenCostNanos(cacheRead, price.rule.CacheReadPerMillion)
	cost.CacheCreationNanos = tokenCostNanos(cacheCreation, price.rule.CacheCreationPerMillion)
	cost.TotalNanos = saturatingSum(
		cost.InputNanos,
		cost.OutputNanos,
		cost.ReasoningNanos,
		cost.CacheReadNanos,
		cost.CacheCreationNanos,
	)
	cost.Total = formatNanos(cost.TotalNanos)
	return pricing, tokens, cost
}

func resolveCounterModes(detail coreusage.Detail, requestedCacheMode, requestedReasoningMode string) (string, string) {
	cacheModes := candidateModes(requestedCacheMode)
	reasoningModes := candidateModes(requestedReasoningMode)
	input := nonNegative(detail.InputTokens)
	output := nonNegative(detail.OutputTokens)
	reasoning := nonNegative(detail.ReasoningTokens)
	cacheRead := nonNegative(detail.CacheReadTokens)
	if cacheRead == 0 {
		cacheRead = nonNegative(detail.CachedTokens)
	}
	cache := saturatingAdd(cacheRead, nonNegative(detail.CacheCreationTokens))
	total := nonNegative(detail.TotalTokens)
	if total == 0 {
		return cacheModes[0], reasoningModes[0]
	}
	base := saturatingAdd(input, output)
	bestCache := cacheModes[0]
	bestReasoning := reasoningModes[0]
	bestDistance := int64(math.MaxInt64)
	for _, cacheMode := range cacheModes {
		for _, reasoningMode := range reasoningModes {
			expected := base
			if cacheMode == modeAdditional {
				expected = saturatingAdd(expected, cache)
			}
			if reasoningMode == modeAdditional {
				expected = saturatingAdd(expected, reasoning)
			}
			distance := absoluteDifference(total, expected)
			if distance < bestDistance {
				bestDistance = distance
				bestCache = cacheMode
				bestReasoning = reasoningMode
			}
		}
	}
	return bestCache, bestReasoning
}

func candidateModes(requested string) []string {
	if requested == modeAdditional {
		return []string{modeAdditional}
	}
	if requested == modeIncluded {
		return []string{modeIncluded}
	}
	return []string{modeIncluded, modeAdditional}
}

func tokenCostNanos(tokens int64, pricePerMillion float64) int64 {
	if tokens <= 0 || pricePerMillion <= 0 {
		return 0
	}
	value := float64(tokens) * pricePerMillion * 1000
	if value >= float64(math.MaxInt64) {
		return math.MaxInt64
	}
	return int64(math.Round(value))
}

func addEventToTotals(totals *Totals, event Event, currency string) {
	if totals == nil {
		return
	}
	totals.Requests++
	if event.Failed {
		totals.Failed++
	} else {
		totals.Success++
	}
	if event.Pricing.Priced {
		totals.PricedRequests++
	} else {
		totals.UnpricedRequests++
	}
	addTokenUsage(&totals.Tokens, event.Tokens)
	addCost(&totals.Cost, event.Cost, currency)
}

func addTokenUsage(dst *TokenUsage, src TokenUsage) {
	dst.InputTokens = saturatingAdd(dst.InputTokens, src.InputTokens)
	dst.OutputTokens = saturatingAdd(dst.OutputTokens, src.OutputTokens)
	dst.ReasoningTokens = saturatingAdd(dst.ReasoningTokens, src.ReasoningTokens)
	dst.CachedTokens = saturatingAdd(dst.CachedTokens, src.CachedTokens)
	dst.CacheReadTokens = saturatingAdd(dst.CacheReadTokens, src.CacheReadTokens)
	dst.CacheCreationTokens = saturatingAdd(dst.CacheCreationTokens, src.CacheCreationTokens)
	dst.TotalTokens = saturatingAdd(dst.TotalTokens, src.TotalTokens)
	dst.BillableInputTokens = saturatingAdd(dst.BillableInputTokens, src.BillableInputTokens)
	dst.BillableOutputTokens = saturatingAdd(dst.BillableOutputTokens, src.BillableOutputTokens)
}

func addCost(dst *CostBreakdown, src CostBreakdown, currency string) {
	dst.Currency = currency
	dst.InputNanos = saturatingAdd(dst.InputNanos, src.InputNanos)
	dst.OutputNanos = saturatingAdd(dst.OutputNanos, src.OutputNanos)
	dst.ReasoningNanos = saturatingAdd(dst.ReasoningNanos, src.ReasoningNanos)
	dst.CacheReadNanos = saturatingAdd(dst.CacheReadNanos, src.CacheReadNanos)
	dst.CacheCreationNanos = saturatingAdd(dst.CacheCreationNanos, src.CacheCreationNanos)
	dst.TotalNanos = saturatingAdd(dst.TotalNanos, src.TotalNanos)
}

func finalizeTotals(totals *Totals, currency string) {
	if totals == nil {
		return
	}
	totals.Cost.Currency = currency
	totals.Cost.Total = formatNanos(totals.Cost.TotalNanos)
}

func loadEvents(file *os.File) ([]Event, error) {
	if _, errSeek := file.Seek(0, 0); errSeek != nil {
		return nil, fmt.Errorf("billing: seek ledger: %w", errSeek)
	}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	events := make([]Event, 0)
	lineNumber := 0
	invalidLines := 0
	for scanner.Scan() {
		lineNumber++
		line := scanner.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var event Event
		if errUnmarshal := json.Unmarshal(line, &event); errUnmarshal != nil {
			invalidLines++
			log.WithFields(log.Fields{"line": lineNumber, "error": errUnmarshal}).Warn("billing: skipped invalid ledger record")
			continue
		}
		events = append(events, event)
	}
	if errScan := scanner.Err(); errScan != nil {
		return nil, fmt.Errorf("billing: read ledger: %w", errScan)
	}
	if invalidLines > 0 {
		log.WithField("invalid_records", invalidLines).Warn("billing: ledger loaded with skipped records")
	}
	if _, errSeek := file.Seek(0, 2); errSeek != nil {
		return nil, fmt.Errorf("billing: seek ledger end: %w", errSeek)
	}
	if errSeparator := ensureLedgerSeparator(file); errSeparator != nil {
		return nil, errSeparator
	}
	return events, nil
}

func normalizeLedgerCurrency(events []Event, currency string) error {
	currency = strings.ToUpper(strings.TrimSpace(currency))
	for i := range events {
		eventCurrency := strings.ToUpper(strings.TrimSpace(events[i].Cost.Currency))
		if eventCurrency == "" {
			events[i].Cost.Currency = currency
			continue
		}
		if eventCurrency != currency {
			return fmt.Errorf("billing: ledger currency %q does not match configured currency %q; use a different store-path", eventCurrency, currency)
		}
		events[i].Cost.Currency = eventCurrency
	}
	return nil
}

func writeLedgerLine(file *os.File, payload []byte) error {
	if file == nil {
		return errors.New("billing: ledger is not open")
	}
	remaining := payload
	for len(remaining) > 0 {
		written, errWrite := file.Write(remaining)
		if written > 0 {
			remaining = remaining[written:]
		}
		if errWrite != nil {
			_, _ = file.Write([]byte{'\n'})
			return errWrite
		}
		if written == 0 {
			_, _ = file.Write([]byte{'\n'})
			return errors.New("billing: ledger write made no progress")
		}
	}
	return nil
}

func ensureLedgerSeparator(file *os.File) error {
	info, errStat := file.Stat()
	if errStat != nil {
		return fmt.Errorf("billing: stat ledger: %w", errStat)
	}
	if info.Size() == 0 {
		return nil
	}
	lastByte := []byte{0}
	if _, errRead := file.ReadAt(lastByte, info.Size()-1); errRead != nil {
		return fmt.Errorf("billing: inspect ledger tail: %w", errRead)
	}
	if lastByte[0] == '\n' {
		return nil
	}
	if _, errWrite := file.Write([]byte{'\n'}); errWrite != nil {
		return fmt.Errorf("billing: repair ledger separator: %w", errWrite)
	}
	return nil
}

func resolveStorePath(configuredPath, authDir, configFilePath string) (string, error) {
	path := strings.TrimSpace(configuredPath)
	if path == "" {
		base := strings.TrimSpace(authDir)
		if base == "" {
			base = filepath.Dir(configFilePath)
		}
		if base == "" || base == "." {
			base = "."
		}
		path = filepath.Join(base, defaultFileName)
	} else if strings.HasPrefix(path, "~"+string(filepath.Separator)) || path == "~" {
		homeDir, errHome := os.UserHomeDir()
		if errHome != nil {
			return "", fmt.Errorf("billing: resolve home directory: %w", errHome)
		}
		path = filepath.Join(homeDir, strings.TrimPrefix(path, "~"+string(filepath.Separator)))
	} else if !filepath.IsAbs(path) {
		base := filepath.Dir(configFilePath)
		if strings.TrimSpace(configFilePath) == "" {
			base = "."
		}
		path = filepath.Join(base, path)
	}
	absPath, errAbs := filepath.Abs(path)
	if errAbs != nil {
		return "", fmt.Errorf("billing: resolve ledger path: %w", errAbs)
	}
	return filepath.Clean(absPath), nil
}

func identifyKey(rawKey string) (string, string) {
	key := strings.TrimSpace(rawKey)
	if key == "" {
		return "anonymous", "anonymous"
	}
	sum := sha256.Sum256([]byte(key))
	mask := "****"
	if len(key) > 4 {
		mask += key[len(key)-4:]
	}
	return hex.EncodeToString(sum[:]), mask
}

func billingSource(rawSource, authType string) (string, string) {
	source := strings.TrimSpace(rawSource)
	if source == "" {
		return "", ""
	}
	normalizedAuthType := strings.ToLower(strings.TrimSpace(authType))
	if strings.Contains(normalizedAuthType, "api") && strings.Contains(normalizedAuthType, "key") {
		sourceID, sourceMask := identifyKey(source)
		return sourceMask, sourceID
	}
	return source, ""
}

func normalizeKeyLabels(labels map[string]string) map[string]string {
	if len(labels) == 0 {
		return nil
	}
	normalized := make(map[string]string, len(labels))
	for rawKey, label := range labels {
		key := strings.TrimSpace(rawKey)
		value := strings.TrimSpace(label)
		if key != "" && value != "" {
			normalized[key] = value
		}
	}
	return normalized
}

func labelsByKeyID(labels map[string]string) map[string]string {
	if len(labels) == 0 {
		return nil
	}
	byID := make(map[string]string, len(labels))
	for rawKey, label := range labels {
		keyID, _ := identifyKey(rawKey)
		byID[keyID] = label
	}
	return byID
}

func normalizeQuery(query Query) Query {
	query.KeyID = strings.ToLower(strings.TrimSpace(query.KeyID))
	query.Provider = strings.ToLower(strings.TrimSpace(query.Provider))
	query.Model = strings.ToLower(strings.TrimSpace(query.Model))
	if query.Limit <= 0 {
		query.Limit = 100
	}
	if query.Limit > 1000 {
		query.Limit = 1000
	}
	if query.Offset < 0 {
		query.Offset = 0
	}
	return query
}

func matchesQuery(event Event, query Query) bool {
	if !query.From.IsZero() && event.Timestamp.Before(query.From) {
		return false
	}
	if !query.To.IsZero() && event.Timestamp.After(query.To) {
		return false
	}
	if query.KeyID != "" && !strings.EqualFold(event.KeyID, query.KeyID) {
		return false
	}
	if query.Provider != "" && !strings.EqualFold(event.Provider, query.Provider) {
		return false
	}
	if query.Model != "" && !strings.EqualFold(event.Model, query.Model) {
		return false
	}
	return true
}

func normalizedValue(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	return value
}

func nonNegative(value int64) int64 {
	if value < 0 {
		return 0
	}
	return value
}

func subtractFloorZero(value, subtract int64) int64 {
	if subtract >= value {
		return 0
	}
	return value - subtract
}

func absoluteDifference(left, right int64) int64 {
	if left >= right {
		return left - right
	}
	return right - left
}

func saturatingAdd(left, right int64) int64 {
	if right > 0 && left > math.MaxInt64-right {
		return math.MaxInt64
	}
	return left + right
}

func saturatingSum(values ...int64) int64 {
	var total int64
	for _, value := range values {
		total = saturatingAdd(total, value)
	}
	return total
}

func formatNanos(nanos int64) string {
	if nanos <= 0 {
		return "0.000000000"
	}
	return fmt.Sprintf("%d.%09d", nanos/1_000_000_000, nanos%1_000_000_000)
}
