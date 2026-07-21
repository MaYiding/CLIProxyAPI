package management

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/billing"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func TestGetBillingUsageReturnsFilteredReport(t *testing.T) {
	manager := billing.NewManager()
	t.Cleanup(func() { _ = manager.Close() })
	if errConfigure := manager.Configure(config.BillingConfig{
		Enabled:   true,
		StorePath: filepath.Join(t.TempDir(), "billing.jsonl"),
		Prices: []config.BillingPrice{{
			Provider:         "openai",
			Model:            "gpt-*",
			InputPerMillion:  1,
			OutputPerMillion: 1,
		}},
	}, t.TempDir(), ""); errConfigure != nil {
		t.Fatalf("Configure() error = %v", errConfigure)
	}
	requestedAt := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	manager.HandleUsage(context.Background(), coreusage.Record{
		Provider: "openai", Model: "gpt-5", APIKey: "client-key", RequestedAt: requestedAt,
		Detail: coreusage.Detail{InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
	})

	handler := &Handler{}
	handler.SetBillingManager(manager)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/billing/usage?provider=openai&model=gpt-5&from=1784548799&to=1784548801&limit=1", nil)
	handler.GetBillingUsage(ginCtx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var report billing.Report
	if errUnmarshal := json.Unmarshal(recorder.Body.Bytes(), &report); errUnmarshal != nil {
		t.Fatalf("Unmarshal() error = %v", errUnmarshal)
	}
	if !report.Enabled || report.MatchedEvents != 1 || len(report.Events) != 1 {
		t.Fatalf("report = %#v", report)
	}
	if report.Totals.Tokens.TotalTokens != 15 || report.Totals.Cost.TotalNanos != 15_000 {
		t.Fatalf("totals = %#v", report.Totals)
	}
}

func TestGetBillingUsageRejectsInvalidRangeAndPagination(t *testing.T) {
	tests := []string{
		"/v0/management/billing/usage?from=200&to=100",
		"/v0/management/billing/usage?limit=0",
		"/v0/management/billing/usage?offset=-1",
		"/v0/management/billing/usage?from=not-a-time",
	}
	for _, target := range tests {
		t.Run(target, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ginCtx, _ := gin.CreateTestContext(recorder)
			ginCtx.Request = httptest.NewRequest(http.MethodGet, target, nil)
			(&Handler{}).GetBillingUsage(ginCtx)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d body=%s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
			}
		})
	}
}

func TestGetBillingSettingsRedactsRawKeysAndShowsQuota(t *testing.T) {
	rawKey := "client-secret-key"
	cfg := &config.Config{
		SDKConfig: config.SDKConfig{APIKeys: []string{rawKey, "unused-key"}},
		Billing: config.BillingConfig{
			Enabled:   true,
			StorePath: filepath.Join(t.TempDir(), "billing.jsonl"),
			KeyLabels: map[string]string{rawKey: "team-a"},
			KeyLimits: map[string]float64{rawKey: 2},
		},
	}
	manager := billing.NewManager()
	if errConfigure := manager.ConfigureForKeys(cfg.Billing, cfg.APIKeys, t.TempDir(), ""); errConfigure != nil {
		t.Fatalf("ConfigureForKeys() error = %v", errConfigure)
	}
	t.Cleanup(func() { _ = manager.Close() })
	manager.HandleUsage(context.Background(), coreusage.Record{
		Provider: "openai", Model: "gpt-5", APIKey: rawKey,
		Detail: coreusage.Detail{InputTokens: 500_000, TotalTokens: 500_000},
	})

	handler := &Handler{cfg: cfg, billingManager: manager}
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/billing/settings", nil)
	handler.GetBillingSettings(ginCtx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), rawKey) || strings.Contains(recorder.Body.String(), "unused-key") {
		t.Fatalf("settings response leaked a raw API key: %s", recorder.Body.String())
	}
	var response billingSettingsResponse
	if errUnmarshal := json.Unmarshal(recorder.Body.Bytes(), &response); errUnmarshal != nil {
		t.Fatalf("Unmarshal() error = %v", errUnmarshal)
	}
	if response.DefaultPricePerMillion != 1 || len(response.Keys) != 2 {
		t.Fatalf("settings response = %#v", response)
	}
	if response.Keys[0].Label != "team-a" || response.Keys[0].Limit != 2 || response.Keys[0].Spent != "0.500000000" || response.Keys[0].Remaining != "1.500000000" || response.Keys[0].Blocked {
		t.Fatalf("first key settings = %#v", response.Keys[0])
	}
}

func TestPutBillingSettingsPersistsAndAppliesImmediately(t *testing.T) {
	tempDir := t.TempDir()
	rawKey := "client-secret-key"
	configPath := filepath.Join(tempDir, "config.yaml")
	initialYAML := "port: 8317\nauth-dir: " + filepath.Join(tempDir, "auth") + "\napi-keys:\n  - " + rawKey + "\nbilling:\n  enabled: true\n"
	if errWrite := os.WriteFile(configPath, []byte(initialYAML), 0o600); errWrite != nil {
		t.Fatalf("WriteFile() error = %v", errWrite)
	}
	cfg := &config.Config{
		SDKConfig: config.SDKConfig{APIKeys: []string{rawKey}},
		AuthDir:   filepath.Join(tempDir, "auth"),
		Billing: config.BillingConfig{
			Enabled:   true,
			StorePath: filepath.Join(tempDir, "billing.jsonl"),
		},
	}
	manager := billing.NewManager()
	if errConfigure := manager.ConfigureForKeys(cfg.Billing, cfg.APIKeys, cfg.AuthDir, configPath); errConfigure != nil {
		t.Fatalf("ConfigureForKeys() error = %v", errConfigure)
	}
	t.Cleanup(func() { _ = manager.Close() })
	manager.HandleUsage(context.Background(), coreusage.Record{
		Provider: "openai", Model: "gpt-5", APIKey: rawKey,
		Detail: coreusage.Detail{InputTokens: 500_000, TotalTokens: 500_000},
	})

	keyID, _ := billing.IdentifyKey(rawKey)
	enabled := true
	syncOnWrite := true
	defaultPrice := 2.0
	prices := []config.BillingPrice{{Name: "gpt", Provider: "openai", Model: "gpt-*", InputPerMillion: 4}}
	keys := []billingKeySettingsUpdate{{KeyID: keyID, Label: "customer-a", Limit: 3}}
	payload, errMarshal := json.Marshal(billingSettingsUpdate{
		Enabled:                &enabled,
		SyncOnWrite:            &syncOnWrite,
		DefaultPricePerMillion: &defaultPrice,
		Prices:                 &prices,
		Keys:                   &keys,
	})
	if errMarshal != nil {
		t.Fatalf("Marshal() error = %v", errMarshal)
	}

	handler := &Handler{cfg: cfg, configFilePath: configPath, billingManager: manager}
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodPut, "/v0/management/billing/settings", bytes.NewReader(payload))
	ginCtx.Request.Header.Set("Content-Type", "application/json")
	handler.PutBillingSettings(ginCtx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), rawKey) {
		t.Fatalf("settings response leaked raw API key: %s", recorder.Body.String())
	}
	persisted, errLoad := config.LoadConfig(configPath)
	if errLoad != nil {
		t.Fatalf("LoadConfig() error = %v", errLoad)
	}
	if !persisted.Billing.Enabled || !persisted.Billing.SyncOnWrite || persisted.Billing.DefaultPricePerMillion == nil || *persisted.Billing.DefaultPricePerMillion != 2 {
		t.Fatalf("persisted billing config = %#v", persisted.Billing)
	}
	if persisted.Billing.KeyLabels[rawKey] != "customer-a" || persisted.Billing.KeyLimits[rawKey] != 3 || len(persisted.Billing.Prices) != 1 {
		t.Fatalf("persisted billing settings = %#v", persisted.Billing)
	}
	status := manager.LimitStatus(rawKey)
	if !status.Allowed || status.Spent != "0.500000000" || status.Remaining != "2.500000000" || status.Limit != "3.000000000" {
		t.Fatalf("immediately applied status = %#v", status)
	}
}

func TestPutBillingSettingsRollsBackWhenLedgerCannotBeApplied(t *testing.T) {
	tempDir := t.TempDir()
	rawKey := "client-secret-key"
	configPath := filepath.Join(tempDir, "config.yaml")
	initialYAML := "port: 8317\napi-keys:\n  - " + rawKey + "\nbilling:\n  enabled: false\n  store-path: " + tempDir + "\n"
	if errWrite := os.WriteFile(configPath, []byte(initialYAML), 0o600); errWrite != nil {
		t.Fatalf("WriteFile() error = %v", errWrite)
	}
	cfg := &config.Config{
		SDKConfig: config.SDKConfig{APIKeys: []string{rawKey}},
		Billing: config.BillingConfig{
			Enabled:   false,
			StorePath: tempDir,
		},
	}
	manager := billing.NewManager()
	t.Cleanup(func() { _ = manager.Close() })

	keyID, _ := billing.IdentifyKey(rawKey)
	enabled := true
	syncOnWrite := false
	defaultPrice := 1.0
	prices := []config.BillingPrice{}
	keys := []billingKeySettingsUpdate{{KeyID: keyID, Limit: 1}}
	payload, errMarshal := json.Marshal(billingSettingsUpdate{
		Enabled:                &enabled,
		SyncOnWrite:            &syncOnWrite,
		DefaultPricePerMillion: &defaultPrice,
		Prices:                 &prices,
		Keys:                   &keys,
	})
	if errMarshal != nil {
		t.Fatalf("Marshal() error = %v", errMarshal)
	}

	handler := &Handler{cfg: cfg, configFilePath: configPath, billingManager: manager}
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodPut, "/v0/management/billing/settings", bytes.NewReader(payload))
	handler.PutBillingSettings(ginCtx)

	if recorder.Code != http.StatusInternalServerError || !strings.Contains(recorder.Body.String(), "rolled back") {
		t.Fatalf("response = %d %s, want rolled-back error", recorder.Code, recorder.Body.String())
	}
	persisted, errLoad := config.LoadConfig(configPath)
	if errLoad != nil {
		t.Fatalf("LoadConfig() error = %v", errLoad)
	}
	if persisted.Billing.Enabled || handler.cfg.Billing.Enabled || manager.Snapshot(billing.Query{}).Enabled {
		t.Fatalf("failed update remained active: persisted=%v in_memory=%v manager=%v", persisted.Billing.Enabled, handler.cfg.Billing.Enabled, manager.Snapshot(billing.Query{}).Enabled)
	}
}

func TestPutBillingSettingsRejectsUnknownKeyAndNegativeAmounts(t *testing.T) {
	rawKey := "client-key"
	keyID, _ := billing.IdentifyKey(rawKey)
	cfg := &config.Config{SDKConfig: config.SDKConfig{APIKeys: []string{rawKey}}}
	handler := &Handler{cfg: cfg}

	tests := []struct {
		name         string
		defaultPrice float64
		key          billingKeySettingsUpdate
		wantStatus   int
	}{
		{name: "unknown key", defaultPrice: 1, key: billingKeySettingsUpdate{KeyID: strings.Repeat("a", 64), Limit: 1}, wantStatus: http.StatusBadRequest},
		{name: "negative default", defaultPrice: -1, key: billingKeySettingsUpdate{KeyID: keyID, Limit: 1}, wantStatus: http.StatusBadRequest},
		{name: "negative limit", defaultPrice: 1, key: billingKeySettingsUpdate{KeyID: keyID, Limit: -1}, wantStatus: http.StatusBadRequest},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			enabled := true
			syncOnWrite := false
			prices := []config.BillingPrice{}
			keys := []billingKeySettingsUpdate{test.key}
			payload, errMarshal := json.Marshal(billingSettingsUpdate{
				Enabled:                &enabled,
				SyncOnWrite:            &syncOnWrite,
				DefaultPricePerMillion: &test.defaultPrice,
				Prices:                 &prices,
				Keys:                   &keys,
			})
			if errMarshal != nil {
				t.Fatalf("Marshal() error = %v", errMarshal)
			}
			recorder := httptest.NewRecorder()
			ginCtx, _ := gin.CreateTestContext(recorder)
			ginCtx.Request = httptest.NewRequest(http.MethodPut, "/v0/management/billing/settings", bytes.NewReader(payload))
			handler.PutBillingSettings(ginCtx)
			if recorder.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d body=%s", recorder.Code, test.wantStatus, recorder.Body.String())
			}
		})
	}
}
