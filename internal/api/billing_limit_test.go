package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/billing"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func TestBillingLimitBlocksEveryAuthenticatedProxyRoute(t *testing.T) {
	server := newTestServer(t)
	manager := billing.DefaultManager()
	if errConfigure := manager.ConfigureForKeys(config.BillingConfig{
		Enabled:   true,
		StorePath: filepath.Join(t.TempDir(), "billing.jsonl"),
		KeyLimits: map[string]float64{"test-key": 1},
	}, []string{"test-key"}, t.TempDir(), ""); errConfigure != nil {
		t.Fatalf("ConfigureForKeys() error = %v", errConfigure)
	}
	t.Cleanup(func() {
		if errConfigure := manager.Configure(config.BillingConfig{}, t.TempDir(), ""); errConfigure != nil {
			t.Errorf("disable billing error = %v", errConfigure)
		}
	})
	coreusage.PublishRecord(context.Background(), coreusage.Record{
		Provider: "openai", Model: "gpt-5", APIKey: "test-key",
		Detail: coreusage.Detail{InputTokens: 1_000_000, TotalTokens: 1_000_000},
	})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && manager.LimitStatus("test-key").Allowed {
		time.Sleep(10 * time.Millisecond)
	}
	if status := manager.LimitStatus("test-key"); status.Allowed {
		t.Fatalf("published usage did not reach billing limiter: %#v", status)
	}

	server.wsAuthEnabled.Store(true)
	server.AttachWebsocketRoute("/v1/quota-test-ws", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	tests := []struct {
		method string
		path   string
	}{
		{method: http.MethodGet, path: "/v1/models"},
		{method: http.MethodPost, path: "/v1/chat/completions"},
		{method: http.MethodPost, path: "/openai/v1/videos"},
		{method: http.MethodPost, path: "/backend-api/codex/responses"},
		{method: http.MethodPost, path: "/v1beta/models/gemini-pro:generateContent"},
		{method: http.MethodGet, path: "/v1/quota-test-ws"},
	}
	for _, test := range tests {
		t.Run(test.method+" "+test.path, func(t *testing.T) {
			req := httptest.NewRequest(test.method, test.path, strings.NewReader(`{"model":"gpt-5"}`))
			req.Header.Set("Authorization", "Bearer test-key")
			req.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()
			server.engine.ServeHTTP(recorder, req)

			if recorder.Code != http.StatusTooManyRequests {
				t.Fatalf("status = %d, want %d body=%s", recorder.Code, http.StatusTooManyRequests, recorder.Body.String())
			}
			if !strings.Contains(recorder.Body.String(), billingLimitExceededCode) || strings.Contains(recorder.Body.String(), "test-key") {
				t.Fatalf("unexpected quota response: %s", recorder.Body.String())
			}
			if recorder.Header().Get("X-Billing-Limit") != "1.000000000" || recorder.Header().Get("X-Billing-Spent") != "1.000000000" {
				t.Fatalf("billing headers = %#v", recorder.Header())
			}
		})
	}
}

func TestBillingLimitAllowsUnlimitedAndDisabledKeys(t *testing.T) {
	manager := billing.NewManager()
	if errConfigure := manager.ConfigureForKeys(config.BillingConfig{
		Enabled:   true,
		StorePath: filepath.Join(t.TempDir(), "billing.jsonl"),
	}, []string{"unlimited-key"}, t.TempDir(), ""); errConfigure != nil {
		t.Fatalf("ConfigureForKeys() error = %v", errConfigure)
	}
	t.Cleanup(func() { _ = manager.Close() })

	engine := newBillingLimitTestEngine(manager, "unlimited-key")
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/proxy", nil))
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("unlimited status = %d, want %d body=%s", recorder.Code, http.StatusNoContent, recorder.Body.String())
	}

	if errConfigure := manager.Configure(config.BillingConfig{}, t.TempDir(), ""); errConfigure != nil {
		t.Fatalf("disable Configure() error = %v", errConfigure)
	}
	recorder = httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/proxy", nil))
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("disabled status = %d, want %d body=%s", recorder.Code, http.StatusNoContent, recorder.Body.String())
	}
}

func newBillingLimitTestEngine(manager *billing.Manager, rawKey string) http.Handler {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.POST("/proxy", func(c *gin.Context) {
		c.Set("userApiKey", rawKey)
		c.Next()
	}, billingLimitMiddleware(manager), func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})
	return engine
}
