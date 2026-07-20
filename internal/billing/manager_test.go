package billing

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	internallogging "github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func TestManagerPricesIncludedCountersAndProtectsKeys(t *testing.T) {
	ledgerPath := filepath.Join(t.TempDir(), "usage.jsonl")
	manager := NewManager()
	t.Cleanup(func() { _ = manager.Close() })
	errConfigure := manager.Configure(config.BillingConfig{
		Enabled:   true,
		StorePath: ledgerPath,
		Currency:  "usd",
		KeyLabels: map[string]string{"client-secret-key": "team-a"},
		Prices: []config.BillingPrice{{
			Name:                    "openai-default",
			Provider:                "openai",
			Model:                   "gpt-*",
			InputPerMillion:         1,
			OutputPerMillion:        2,
			ReasoningPerMillion:     3,
			CacheReadPerMillion:     0.5,
			CacheCreationPerMillion: 0.75,
		}},
	}, t.TempDir(), "")
	if errConfigure != nil {
		t.Fatalf("Configure() error = %v", errConfigure)
	}

	ctx := internallogging.WithRequestID(context.Background(), "request-1")
	ctx = internallogging.WithEndpoint(ctx, "POST /v1/responses")
	manager.HandleUsage(ctx, coreusage.Record{
		Provider:    "openai",
		Model:       "gpt-5",
		APIKey:      "client-secret-key",
		AuthType:    "api_key",
		Source:      "upstream-secret-key",
		RequestedAt: time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC),
		Detail: coreusage.Detail{
			InputTokens:     100,
			OutputTokens:    50,
			ReasoningTokens: 20,
			CacheReadTokens: 40,
			CachedTokens:    40,
			TotalTokens:     150,
		},
	})

	report := manager.Snapshot(Query{Limit: 10})
	if report.MatchedEvents != 1 || len(report.Events) != 1 {
		t.Fatalf("event counts = %d/%d, want 1/1", report.MatchedEvents, len(report.Events))
	}
	event := report.Events[0]
	if event.KeyLabel != "team-a" || event.KeyMask != "****-key" {
		t.Fatalf("key identity = %q/%q, want team-a/****-key", event.KeyLabel, event.KeyMask)
	}
	if event.KeyID == "" || event.KeyID == "client-secret-key" {
		t.Fatalf("key id = %q, want non-raw identifier", event.KeyID)
	}
	if event.Source != "****-key" || event.SourceID == "" {
		t.Fatalf("source identity = %q/%q, want masked source and hash", event.Source, event.SourceID)
	}
	if event.Tokens.BillableInputTokens != 60 || event.Tokens.BillableOutputTokens != 30 {
		t.Fatalf("billable tokens = %d/%d, want 60/30", event.Tokens.BillableInputTokens, event.Tokens.BillableOutputTokens)
	}
	if event.Pricing.InputCacheMode != modeIncluded || event.Pricing.ReasoningMode != modeIncluded {
		t.Fatalf("resolved modes = %q/%q, want included/included", event.Pricing.InputCacheMode, event.Pricing.ReasoningMode)
	}
	if event.Cost.TotalNanos != 200_000 || event.Cost.Total != "0.000200000" {
		t.Fatalf("cost = %d/%q, want 200000/0.000200000", event.Cost.TotalNanos, event.Cost.Total)
	}

	ledger, errRead := os.ReadFile(ledgerPath)
	if errRead != nil {
		t.Fatalf("ReadFile() error = %v", errRead)
	}
	if strings.Contains(string(ledger), "client-secret-key") || strings.Contains(string(ledger), "upstream-secret-key") {
		t.Fatalf("ledger persisted a raw API key: %s", ledger)
	}
}

func TestManagerAutoDetectsAdditionalCacheCounters(t *testing.T) {
	manager := newTestManager(t, config.BillingConfig{
		Enabled: true,
		Prices: []config.BillingPrice{{
			Provider:                "claude",
			Model:                   "claude-*",
			InputPerMillion:         1,
			OutputPerMillion:        1,
			CacheReadPerMillion:     1,
			CacheCreationPerMillion: 1,
		}},
	})
	manager.HandleUsage(context.Background(), coreusage.Record{
		Provider: "claude",
		Model:    "claude-sonnet",
		APIKey:   "client-b",
		Detail: coreusage.Detail{
			InputTokens:         10,
			OutputTokens:        20,
			CacheReadTokens:     30,
			CacheCreationTokens: 40,
			TotalTokens:         100,
		},
	})

	event := manager.Snapshot(Query{Limit: 10}).Events[0]
	if event.Pricing.InputCacheMode != modeAdditional {
		t.Fatalf("input cache mode = %q, want additional", event.Pricing.InputCacheMode)
	}
	if event.Tokens.BillableInputTokens != 10 || event.Tokens.BillableOutputTokens != 20 {
		t.Fatalf("billable tokens = %d/%d, want 10/20", event.Tokens.BillableInputTokens, event.Tokens.BillableOutputTokens)
	}
	if event.Cost.TotalNanos != 100_000 {
		t.Fatalf("cost nanos = %d, want 100000", event.Cost.TotalNanos)
	}
}

func TestManagerReloadsLedgerAndFiltersAllAggregates(t *testing.T) {
	ledgerPath := filepath.Join(t.TempDir(), "billing.jsonl")
	cfg := config.BillingConfig{
		Enabled:   true,
		StorePath: ledgerPath,
		Prices: []config.BillingPrice{{
			Provider:         "*",
			Model:            "gpt-*",
			InputPerMillion:  1,
			OutputPerMillion: 1,
		}},
	}
	manager := NewManager()
	if errConfigure := manager.Configure(cfg, t.TempDir(), ""); errConfigure != nil {
		t.Fatalf("Configure() error = %v", errConfigure)
	}
	firstTime := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	secondTime := firstTime.Add(time.Hour)
	manager.HandleUsage(context.Background(), coreusage.Record{
		Provider: "openai", Model: "gpt-5", APIKey: "key-a", RequestedAt: firstTime,
		Detail: coreusage.Detail{InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
	})
	manager.HandleUsage(context.Background(), coreusage.Record{
		Provider: "gemini", Model: "gemini-pro", APIKey: "key-b", RequestedAt: secondTime, Failed: true,
		Detail: coreusage.Detail{InputTokens: 7, OutputTokens: 3, TotalTokens: 10},
	})
	if errClose := manager.Close(); errClose != nil {
		t.Fatalf("Close() error = %v", errClose)
	}
	if errConfigure := manager.Configure(cfg, t.TempDir(), ""); errConfigure != nil {
		t.Fatalf("reload Configure() error = %v", errConfigure)
	}
	t.Cleanup(func() { _ = manager.Close() })

	keyAID, _ := identifyKey("key-a")
	report := manager.Snapshot(Query{
		From:   firstTime.Add(-time.Minute),
		To:     firstTime.Add(time.Minute),
		KeyID:  keyAID,
		Model:  "gpt-5",
		Limit:  1,
		Offset: 0,
	})
	if report.LedgerEvents != 2 || report.MatchedEvents != 1 || len(report.Events) != 1 {
		t.Fatalf("counts = ledger:%d matched:%d page:%d, want 2/1/1", report.LedgerEvents, report.MatchedEvents, len(report.Events))
	}
	if report.Totals.Requests != 1 || report.Totals.PricedRequests != 1 || report.Totals.UnpricedRequests != 0 {
		t.Fatalf("totals = %#v", report.Totals)
	}
	if len(report.ByKey) != 1 || len(report.ByModel) != 1 {
		t.Fatalf("group counts = %d/%d, want 1/1", len(report.ByKey), len(report.ByModel))
	}

	all := manager.Snapshot(Query{Limit: 1, Offset: 1})
	if all.MatchedEvents != 2 || len(all.Events) != 1 || !all.Events[0].Timestamp.Equal(firstTime) {
		t.Fatalf("reverse pagination mismatch: %#v", all.Events)
	}
	if all.Totals.PricedRequests != 1 || all.Totals.UnpricedRequests != 1 || all.Totals.Failed != 1 {
		t.Fatalf("all totals = %#v", all.Totals)
	}

	cfg.KeyLabels = map[string]string{"key-a": "renamed-customer"}
	if errConfigure := manager.Configure(cfg, t.TempDir(), ""); errConfigure != nil {
		t.Fatalf("label reload Configure() error = %v", errConfigure)
	}
	renamed := manager.Snapshot(Query{KeyID: keyAID, Limit: 10})
	if len(renamed.Events) != 1 || renamed.Events[0].KeyLabel != "renamed-customer" || renamed.ByKey[0].KeyLabel != "renamed-customer" {
		t.Fatalf("updated key label not applied to historical report: %#v", renamed)
	}
}

func TestConfigureRejectsInvalidPricing(t *testing.T) {
	manager := NewManager()
	errConfigure := manager.Configure(config.BillingConfig{
		Enabled: true,
		Prices: []config.BillingPrice{{
			InputPerMillion: -1,
		}},
	}, t.TempDir(), "")
	if errConfigure == nil || !strings.Contains(errConfigure.Error(), "non-negative") {
		t.Fatalf("Configure() error = %v, want non-negative price error", errConfigure)
	}

	errConfigure = manager.Configure(config.BillingConfig{
		Enabled: true,
		Prices: []config.BillingPrice{{
			InputCacheMode: "sometimes",
		}},
	}, t.TempDir(), "")
	if errConfigure == nil || !strings.Contains(errConfigure.Error(), "input-cache-mode") {
		t.Fatalf("Configure() error = %v, want mode error", errConfigure)
	}
}

func TestConfigureRejectsLedgerCurrencyMismatch(t *testing.T) {
	ledgerPath := filepath.Join(t.TempDir(), "billing.jsonl")
	manager := NewManager()
	if errConfigure := manager.Configure(config.BillingConfig{
		Enabled:   true,
		StorePath: ledgerPath,
		Currency:  "USD",
	}, t.TempDir(), ""); errConfigure != nil {
		t.Fatalf("Configure() error = %v", errConfigure)
	}
	manager.HandleUsage(context.Background(), coreusage.Record{
		Provider: "openai", Model: "gpt-5", APIKey: "key-a",
		Detail: coreusage.Detail{InputTokens: 1, TotalTokens: 1},
	})
	if errClose := manager.Close(); errClose != nil {
		t.Fatalf("Close() error = %v", errClose)
	}

	errConfigure := manager.Configure(config.BillingConfig{
		Enabled:   true,
		StorePath: ledgerPath,
		Currency:  "EUR",
	}, t.TempDir(), "")
	if errConfigure == nil || !strings.Contains(errConfigure.Error(), "ledger currency") || !strings.Contains(errConfigure.Error(), "different store-path") {
		t.Fatalf("Configure() error = %v, want ledger currency mismatch", errConfigure)
	}
}

func TestManagerRepairsCorruptLedgerTailBeforeAppending(t *testing.T) {
	ledgerPath := filepath.Join(t.TempDir(), "billing.jsonl")
	cfg := config.BillingConfig{Enabled: true, StorePath: ledgerPath}
	manager := NewManager()
	if errConfigure := manager.Configure(cfg, t.TempDir(), ""); errConfigure != nil {
		t.Fatalf("Configure() error = %v", errConfigure)
	}
	manager.HandleUsage(context.Background(), coreusage.Record{
		Provider: "openai", Model: "gpt-5", APIKey: "key-a",
		Detail: coreusage.Detail{InputTokens: 1, TotalTokens: 1},
	})
	if errClose := manager.Close(); errClose != nil {
		t.Fatalf("Close() error = %v", errClose)
	}

	file, errOpen := os.OpenFile(ledgerPath, os.O_WRONLY|os.O_APPEND, 0o600)
	if errOpen != nil {
		t.Fatalf("OpenFile() error = %v", errOpen)
	}
	if _, errWrite := file.Write([]byte(`{"partial":`)); errWrite != nil {
		_ = file.Close()
		t.Fatalf("Write() error = %v", errWrite)
	}
	if errClose := file.Close(); errClose != nil {
		t.Fatalf("corrupt file Close() error = %v", errClose)
	}

	reloaded := NewManager()
	if errConfigure := reloaded.Configure(cfg, t.TempDir(), ""); errConfigure != nil {
		t.Fatalf("reload Configure() error = %v", errConfigure)
	}
	reloaded.HandleUsage(context.Background(), coreusage.Record{
		Provider: "claude", Model: "claude-sonnet", APIKey: "key-b",
		Detail: coreusage.Detail{InputTokens: 2, TotalTokens: 2},
	})
	if errClose := reloaded.Close(); errClose != nil {
		t.Fatalf("reloaded Close() error = %v", errClose)
	}

	verified := NewManager()
	if errConfigure := verified.Configure(cfg, t.TempDir(), ""); errConfigure != nil {
		t.Fatalf("verified Configure() error = %v", errConfigure)
	}
	t.Cleanup(func() { _ = verified.Close() })
	report := verified.Snapshot(Query{Limit: 10})
	if report.LedgerEvents != 2 || len(report.Events) != 2 {
		t.Fatalf("events = %d/%d, want 2/2", report.LedgerEvents, len(report.Events))
	}
	ledger, errRead := os.ReadFile(ledgerPath)
	if errRead != nil {
		t.Fatalf("ReadFile() error = %v", errRead)
	}
	if !strings.Contains(string(ledger), `{"partial":`+"\n"+`{"id":`) {
		t.Fatalf("corrupt tail was not isolated before next event: %s", ledger)
	}
}

func TestDefaultManagerReceivesPublishedUsageRecords(t *testing.T) {
	manager := DefaultManager()
	if errConfigure := manager.Configure(config.BillingConfig{
		Enabled:   true,
		StorePath: filepath.Join(t.TempDir(), "billing.jsonl"),
	}, t.TempDir(), ""); errConfigure != nil {
		t.Fatalf("Configure() error = %v", errConfigure)
	}
	t.Cleanup(func() {
		if errConfigure := manager.Configure(config.BillingConfig{}, t.TempDir(), ""); errConfigure != nil {
			t.Errorf("disable Configure() error = %v", errConfigure)
		}
	})

	coreusage.PublishRecord(context.Background(), coreusage.Record{
		Provider: "openai", Model: "gpt-5", APIKey: "published-key",
		Detail: coreusage.Detail{InputTokens: 3, TotalTokens: 3},
	})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		report := manager.Snapshot(Query{Limit: 10})
		if report.LedgerEvents == 1 {
			if len(report.Events) != 1 || report.Events[0].Tokens.InputTokens != 3 {
				t.Fatalf("published event = %#v", report.Events)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for default usage manager to dispatch billing event")
}

func newTestManager(t *testing.T, cfg config.BillingConfig) *Manager {
	t.Helper()
	manager := NewManager()
	if cfg.StorePath == "" {
		cfg.StorePath = filepath.Join(t.TempDir(), "billing.jsonl")
	}
	if errConfigure := manager.Configure(cfg, t.TempDir(), ""); errConfigure != nil {
		t.Fatalf("Configure() error = %v", errConfigure)
	}
	t.Cleanup(func() { _ = manager.Close() })
	return manager
}
