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
	if all.Totals.PricedRequests != 2 || all.Totals.UnpricedRequests != 0 || all.Totals.Failed != 1 || all.Totals.Cost.TotalNanos != 25_000 {
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

func TestManagerHotUpdatesMetadataWithoutReplayingLedger(t *testing.T) {
	ledgerPath := filepath.Join(t.TempDir(), "billing.jsonl")
	initialDefault := 1.0
	cfg := config.BillingConfig{
		Enabled:                true,
		StorePath:              ledgerPath,
		Currency:               "USD",
		DefaultPricePerMillion: &initialDefault,
		KeyLabels:              map[string]string{"hot-key": "old-name"},
		KeyLimits:              map[string]float64{"hot-key": 5},
		Prices: []config.BillingPrice{{
			Name:            "old-openai-price",
			Provider:        "openai",
			Model:           "gpt-*",
			InputPerMillion: 1,
		}},
	}
	manager := NewManager()
	if errConfigure := manager.ConfigureForKeys(cfg, []string{"hot-key"}, t.TempDir(), ""); errConfigure != nil {
		t.Fatalf("initial ConfigureForKeys() error = %v", errConfigure)
	}
	t.Cleanup(func() { _ = manager.Close() })

	baseTime := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)
	manager.HandleUsage(context.Background(), coreusage.Record{
		Provider: "openai", Model: "gpt-5", APIKey: "hot-key", RequestedAt: baseTime,
		Detail: coreusage.Detail{InputTokens: 1_000_000, TotalTokens: 1_000_000},
	})
	fileBeforeUpdate := manager.file
	if fileBeforeUpdate == nil {
		t.Fatal("manager.file = nil after initial configuration")
	}

	ledger, errOpen := os.OpenFile(ledgerPath, os.O_WRONLY|os.O_APPEND, 0o600)
	if errOpen != nil {
		t.Fatalf("os.OpenFile() error = %v", errOpen)
	}
	if _, errWrite := ledger.WriteString("malformed-ledger-record\n"); errWrite != nil {
		_ = ledger.Close()
		t.Fatalf("ledger.WriteString() error = %v", errWrite)
	}
	if errClose := ledger.Close(); errClose != nil {
		t.Fatalf("ledger.Close() error = %v", errClose)
	}

	updatedDefault := 4.0
	cfg.SyncOnWrite = true
	cfg.DefaultPricePerMillion = &updatedDefault
	cfg.KeyLabels = map[string]string{"hot-key": "new-name"}
	cfg.KeyLimits = map[string]float64{"hot-key": 10}
	cfg.Prices = []config.BillingPrice{{
		Name:            "new-openai-price",
		Provider:        "openai",
		Model:           "gpt-*",
		InputPerMillion: 2,
	}}
	if errConfigure := manager.ConfigureForKeys(cfg, []string{"hot-key"}, t.TempDir(), ""); errConfigure != nil {
		t.Fatalf("metadata ConfigureForKeys() error = %v", errConfigure)
	}
	if manager.file != fileBeforeUpdate {
		t.Fatal("metadata update replaced the open ledger file, indicating a replay")
	}

	afterUpdate := manager.Snapshot(Query{Limit: 10})
	if afterUpdate.LedgerEvents != 1 || len(afterUpdate.Events) != 1 {
		t.Fatalf("events after metadata update = %d/%d, want 1/1", afterUpdate.LedgerEvents, len(afterUpdate.Events))
	}
	if afterUpdate.Events[0].KeyLabel != "new-name" || afterUpdate.Events[0].Cost.TotalNanos != 1_000_000_000 {
		t.Fatalf("historical event after metadata update = %#v", afterUpdate.Events[0])
	}
	if !afterUpdate.SyncOnWrite || afterUpdate.DefaultPricePerMillion != 4 {
		t.Fatalf("live settings after update = sync:%v default:%v", afterUpdate.SyncOnWrite, afterUpdate.DefaultPricePerMillion)
	}
	status := manager.LimitStatus("hot-key")
	if status.KeyLabel != "new-name" || !status.Limited || status.Limit != "10.000000000" || status.Spent != "1.000000000" || status.Remaining != "9.000000000" {
		t.Fatalf("limit status after metadata update = %#v", status)
	}

	manager.HandleUsage(context.Background(), coreusage.Record{
		Provider: "openai", Model: "gpt-5", APIKey: "hot-key", RequestedAt: baseTime.Add(time.Minute),
		Detail: coreusage.Detail{InputTokens: 1_000_000, TotalTokens: 1_000_000},
	})
	manager.HandleUsage(context.Background(), coreusage.Record{
		Provider: "gemini", Model: "gemini-pro", APIKey: "hot-key", RequestedAt: baseTime.Add(2 * time.Minute),
		Detail: coreusage.Detail{InputTokens: 1_000_000, TotalTokens: 1_000_000},
	})

	final := manager.Snapshot(Query{Limit: 10})
	if final.LedgerEvents != 3 || len(final.Events) != 3 || final.Totals.Cost.TotalNanos != 7_000_000_000 {
		t.Fatalf("final report = events:%d/%d cost:%d, want 3/3/7000000000", final.LedgerEvents, len(final.Events), final.Totals.Cost.TotalNanos)
	}
	if final.Events[0].Pricing.Rule != "default" || final.Events[0].Cost.TotalNanos != 4_000_000_000 {
		t.Fatalf("new default-priced event = %#v", final.Events[0])
	}
	if final.Events[1].Pricing.Rule != "new-openai-price" || final.Events[1].Cost.TotalNanos != 2_000_000_000 {
		t.Fatalf("new rule-priced event = %#v", final.Events[1])
	}
	if final.Events[2].Pricing.Rule != "old-openai-price" || final.Events[2].Cost.TotalNanos != 1_000_000_000 {
		t.Fatalf("historical priced event changed = %#v", final.Events[2])
	}
	status = manager.LimitStatus("hot-key")
	if status.Spent != "7.000000000" || status.Remaining != "3.000000000" || status.Blocked {
		t.Fatalf("final limit status = %#v", status)
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

	invalidDefault := -1.0
	errConfigure = manager.Configure(config.BillingConfig{
		Enabled:                true,
		DefaultPricePerMillion: &invalidDefault,
	}, t.TempDir(), "")
	if errConfigure == nil || !strings.Contains(errConfigure.Error(), "default price") {
		t.Fatalf("Configure() error = %v, want default price error", errConfigure)
	}

	errConfigure = manager.Configure(config.BillingConfig{
		Enabled:   true,
		KeyLimits: map[string]float64{"client-key": -1},
	}, t.TempDir(), "")
	if errConfigure == nil || !strings.Contains(errConfigure.Error(), "key limit") {
		t.Fatalf("Configure() error = %v, want key limit error", errConfigure)
	}

	errConfigure = manager.Configure(config.BillingConfig{
		Enabled:   true,
		KeyLimits: map[string]float64{"client-key": 0.0000000001},
	}, t.TempDir(), "")
	if errConfigure == nil || !strings.Contains(errConfigure.Error(), "at least 0.000000001") {
		t.Fatalf("Configure() error = %v, want minimum key limit error", errConfigure)
	}
}

func TestManagerUsesConfigurableDefaultPrice(t *testing.T) {
	ledgerPath := filepath.Join(t.TempDir(), "billing.jsonl")
	manager := NewManager()
	if errConfigure := manager.Configure(config.BillingConfig{Enabled: true, StorePath: ledgerPath}, t.TempDir(), ""); errConfigure != nil {
		t.Fatalf("Configure() error = %v", errConfigure)
	}
	manager.HandleUsage(context.Background(), coreusage.Record{
		Provider: "gemini", Model: "gemini-pro", APIKey: "key-a",
		Detail: coreusage.Detail{InputTokens: 600_000, OutputTokens: 400_000, TotalTokens: 1_000_000},
	})

	first := manager.Snapshot(Query{Limit: 10})
	if first.DefaultPricePerMillion != 1 || len(first.Events) != 1 {
		t.Fatalf("default-priced report = %#v", first)
	}
	if first.Events[0].Pricing.Rule != "default" || first.Events[0].Cost.TotalNanos != 1_000_000_000 || first.Events[0].Cost.Total != "1.000000000" {
		t.Fatalf("default-priced event = %#v", first.Events[0])
	}

	customDefault := 2.5
	if errConfigure := manager.Configure(config.BillingConfig{
		Enabled:                true,
		StorePath:              ledgerPath,
		DefaultPricePerMillion: &customDefault,
	}, t.TempDir(), ""); errConfigure != nil {
		t.Fatalf("custom Configure() error = %v", errConfigure)
	}
	manager.HandleUsage(context.Background(), coreusage.Record{
		Provider: "claude", Model: "claude-sonnet", APIKey: "key-a",
		Detail: coreusage.Detail{InputTokens: 1_000_000, TotalTokens: 1_000_000},
	})
	t.Cleanup(func() { _ = manager.Close() })

	second := manager.Snapshot(Query{Limit: 10})
	if second.DefaultPricePerMillion != 2.5 || second.Totals.Cost.TotalNanos != 3_500_000_000 {
		t.Fatalf("custom default report = %#v", second)
	}
	if second.Events[0].Cost.TotalNanos != 2_500_000_000 || second.Events[1].Cost.TotalNanos != 1_000_000_000 {
		t.Fatalf("historical prices were not frozen: %#v", second.Events)
	}
}

func TestManagerChargesReportedTotalWhenBreakdownIsMissing(t *testing.T) {
	manager := newTestManager(t, config.BillingConfig{Enabled: true})
	manager.HandleUsage(context.Background(), coreusage.Record{
		Provider: "custom", Model: "total-only", APIKey: "key-a",
		Detail: coreusage.Detail{TotalTokens: 1_000_000},
	})

	event := manager.Snapshot(Query{Limit: 10}).Events[0]
	if event.Tokens.InputTokens != 0 || event.Tokens.BillableInputTokens != 1_000_000 || event.Tokens.TotalTokens != 1_000_000 {
		t.Fatalf("total-only tokens = %#v", event.Tokens)
	}
	if event.Cost.TotalNanos != 1_000_000_000 || event.Cost.Total != "1.000000000" {
		t.Fatalf("total-only cost = %#v", event.Cost)
	}
}

func TestManagerEnforcesConfiguredKeyLimitAndReloadsSpend(t *testing.T) {
	ledgerPath := filepath.Join(t.TempDir(), "billing.jsonl")
	cfg := config.BillingConfig{
		Enabled:   true,
		StorePath: ledgerPath,
		KeyLabels: map[string]string{"limited-key": "team-limited"},
		KeyLimits: map[string]float64{"limited-key": 1},
	}
	manager := NewManager()
	if errConfigure := manager.ConfigureForKeys(cfg, []string{"limited-key", "unlimited-key"}, t.TempDir(), ""); errConfigure != nil {
		t.Fatalf("ConfigureForKeys() error = %v", errConfigure)
	}

	initial := manager.Snapshot(Query{Limit: 10})
	if len(initial.ByKey) != 2 {
		t.Fatalf("configured keys = %d, want 2: %#v", len(initial.ByKey), initial.ByKey)
	}
	if status := manager.LimitStatus("limited-key"); !status.Allowed || !status.Limited || status.Blocked || status.Limit != "1.000000000" {
		t.Fatalf("initial limited status = %#v", status)
	}

	manager.HandleUsage(context.Background(), coreusage.Record{
		Provider: "openai", Model: "gpt-5", APIKey: "limited-key",
		Detail: coreusage.Detail{InputTokens: 1_000_000, TotalTokens: 1_000_000},
	})
	status := manager.LimitStatus("limited-key")
	if status.Allowed || !status.Blocked || status.Spent != "1.000000000" || status.Remaining != "0.000000000" {
		t.Fatalf("exact-limit status = %#v", status)
	}
	if status := manager.LimitStatus("unlimited-key"); !status.Allowed || status.Limited || status.Blocked {
		t.Fatalf("unlimited status = %#v", status)
	}

	cfg.KeyLimits["limited-key"] = 2
	if errConfigure := manager.ConfigureForKeys(cfg, []string{"limited-key", "unlimited-key"}, t.TempDir(), ""); errConfigure != nil {
		t.Fatalf("increased-limit ConfigureForKeys() error = %v", errConfigure)
	}
	if status := manager.LimitStatus("limited-key"); !status.Allowed || status.Blocked || status.Remaining != "1.000000000" {
		t.Fatalf("increased-limit status = %#v", status)
	}
	if errClose := manager.Close(); errClose != nil {
		t.Fatalf("Close() error = %v", errClose)
	}

	cfg.KeyLimits["limited-key"] = 0.5
	reloaded := NewManager()
	if errConfigure := reloaded.ConfigureForKeys(cfg, []string{"limited-key", "unlimited-key"}, t.TempDir(), ""); errConfigure != nil {
		t.Fatalf("reload ConfigureForKeys() error = %v", errConfigure)
	}
	t.Cleanup(func() { _ = reloaded.Close() })
	if status := reloaded.LimitStatus("limited-key"); status.Allowed || !status.Blocked || status.Spent != "1.000000000" {
		t.Fatalf("reloaded status = %#v", status)
	}
	report := reloaded.Snapshot(Query{Limit: 10})
	if len(report.ByKey) != 2 || !report.ByKey[0].Quota.Blocked {
		t.Fatalf("reloaded key summaries = %#v", report.ByKey)
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
