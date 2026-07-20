package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
