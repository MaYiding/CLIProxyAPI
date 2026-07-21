package management

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestGetAPIKeysConcurrentWithConfigReplacement(t *testing.T) {
	h := &Handler{cfg: &config.Config{SDKConfig: config.SDKConfig{APIKeys: []string{"key-a"}}}}
	start := make(chan struct{})
	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		<-start
		for i := 0; i < 500; i++ {
			keys := []string{"key-a"}
			if i%2 == 0 {
				keys = []string{"key-b", "key-c"}
			}
			h.SetConfig(&config.Config{SDKConfig: config.SDKConfig{APIKeys: keys}})
		}
	}()
	go func() {
		defer workers.Done()
		<-start
		for i := 0; i < 500; i++ {
			recorder := httptest.NewRecorder()
			ginCtx, _ := gin.CreateTestContext(recorder)
			ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/api-keys", nil)
			h.GetAPIKeys(ginCtx)
		}
	}()
	close(start)
	workers.Wait()
}

func TestPutAPIKeysPrunesBillingMetadata(t *testing.T) {
	h := newAPIKeysBillingTestHandler(t, []string{"key-a", "key-b"})

	recorder := runAPIKeysRequest(t, h, http.MethodPut, "/v0/management/api-keys", `{"items":["key-b","key-c"]}`)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	assertAPIKeysBillingState(t, h,
		[]string{"key-b", "key-c"},
		map[string]string{"key-b": "team-b"},
		map[string]float64{"key-b": 2},
	)
	assertPersistedAPIKeysBillingState(t, h.configFilePath,
		[]string{"key-b", "key-c"},
		map[string]string{"key-b": "team-b"},
		map[string]float64{"key-b": 2},
	)
}

func TestPutAPIKeysRollsBackWhenPersistenceFails(t *testing.T) {
	h := newAPIKeysBillingTestHandler(t, []string{"key-a", "key-b"})
	h.configFilePath = filepath.Join(t.TempDir(), "missing", "config.yaml")

	recorder := runAPIKeysRequest(t, h, http.MethodPut, "/v0/management/api-keys", `{"items":["key-b","key-c"]}`)

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusInternalServerError, recorder.Body.String())
	}
	assertAPIKeysBillingState(t, h,
		[]string{"key-a", "key-b"},
		map[string]string{
			" key-b ":    "legacy-spaced-team-b",
			"key-a":      "team-a",
			"key-b":      "team-b",
			"orphan-key": "orphan",
		},
		map[string]float64{
			" key-b ":    22,
			"key-a":      1,
			"key-b":      2,
			"orphan-key": 9,
		},
	)
}

func TestPatchAPIKeysPrunesReplacedKeyBillingMetadata(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "old and new", body: `{"old":"key-a","new":"key-c"}`},
		{name: "index and value", body: `{"index":0,"value":"key-c"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newAPIKeysBillingTestHandler(t, []string{"key-a", "key-b"})

			recorder := runAPIKeysRequest(t, h, http.MethodPatch, "/v0/management/api-keys", tt.body)

			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
			}
			assertAPIKeysBillingState(t, h,
				[]string{"key-c", "key-b"},
				map[string]string{"key-b": "team-b"},
				map[string]float64{"key-b": 2},
			)
		})
	}
}

func TestDeleteAPIKeysKeepsMetadataUntilKeyIsNoLongerActive(t *testing.T) {
	t.Run("index keeps duplicate key metadata", func(t *testing.T) {
		h := newAPIKeysBillingTestHandler(t, []string{"key-a", "key-a", "key-b"})

		recorder := runAPIKeysRequest(t, h, http.MethodDelete, "/v0/management/api-keys?index=0", "")

		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
		}
		assertAPIKeysBillingState(t, h,
			[]string{"key-a", "key-b"},
			map[string]string{"key-a": "team-a", "key-b": "team-b"},
			map[string]float64{"key-a": 1, "key-b": 2},
		)
	})

	t.Run("value removes all matches and metadata", func(t *testing.T) {
		h := newAPIKeysBillingTestHandler(t, []string{"key-a", " key-a ", "key-b"})

		recorder := runAPIKeysRequest(t, h, http.MethodDelete, "/v0/management/api-keys?value=key-a", "")

		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
		}
		assertAPIKeysBillingState(t, h,
			[]string{"key-b"},
			map[string]string{"key-b": "team-b"},
			map[string]float64{"key-b": 2},
		)
		assertPersistedAPIKeysBillingState(t, h.configFilePath,
			[]string{"key-b"},
			map[string]string{"key-b": "team-b"},
			map[string]float64{"key-b": 2},
		)
	})
}

func newAPIKeysBillingTestHandler(t *testing.T, apiKeys []string) *Handler {
	t.Helper()
	return &Handler{
		cfg: &config.Config{
			SDKConfig: config.SDKConfig{APIKeys: append([]string(nil), apiKeys...)},
			Billing: config.BillingConfig{
				KeyLabels: map[string]string{
					" key-b ":    "legacy-spaced-team-b",
					"key-a":      "team-a",
					"key-b":      "team-b",
					"orphan-key": "orphan",
				},
				KeyLimits: map[string]float64{
					" key-b ":    22,
					"key-a":      1,
					"key-b":      2,
					"orphan-key": 9,
				},
			},
		},
		configFilePath: writeTestConfigFile(t),
	}
}

func runAPIKeysRequest(t *testing.T, h *Handler, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(method, target, strings.NewReader(body))
	if body != "" {
		ginCtx.Request.Header.Set("Content-Type", "application/json")
	}
	switch method {
	case http.MethodPut:
		h.PutAPIKeys(ginCtx)
	case http.MethodPatch:
		h.PatchAPIKeys(ginCtx)
	case http.MethodDelete:
		h.DeleteAPIKeys(ginCtx)
	default:
		t.Fatalf("unsupported method %q", method)
	}
	return recorder
}

func assertAPIKeysBillingState(t *testing.T, h *Handler, wantKeys []string, wantLabels map[string]string, wantLimits map[string]float64) {
	t.Helper()
	h.mu.Lock()
	gotKeys := append([]string(nil), h.cfg.APIKeys...)
	gotLabels := cloneStringMap(h.cfg.Billing.KeyLabels)
	gotLimits := cloneFloatMap(h.cfg.Billing.KeyLimits)
	h.mu.Unlock()

	if !reflect.DeepEqual(gotKeys, wantKeys) {
		t.Errorf("api keys = %#v, want %#v", gotKeys, wantKeys)
	}
	if !reflect.DeepEqual(gotLabels, wantLabels) {
		t.Errorf("billing key labels = %#v, want %#v", gotLabels, wantLabels)
	}
	if !reflect.DeepEqual(gotLimits, wantLimits) {
		t.Errorf("billing key limits = %#v, want %#v", gotLimits, wantLimits)
	}
}

func assertPersistedAPIKeysBillingState(t *testing.T, configPath string, wantKeys []string, wantLabels map[string]string, wantLimits map[string]float64) {
	t.Helper()
	persisted, errLoad := config.LoadConfig(configPath)
	if errLoad != nil {
		t.Fatalf("LoadConfig() error = %v", errLoad)
	}
	if !reflect.DeepEqual(persisted.APIKeys, wantKeys) {
		t.Errorf("persisted api keys = %#v, want %#v", persisted.APIKeys, wantKeys)
	}
	if !reflect.DeepEqual(persisted.Billing.KeyLabels, wantLabels) {
		t.Errorf("persisted billing key labels = %#v, want %#v", persisted.Billing.KeyLabels, wantLabels)
	}
	if !reflect.DeepEqual(persisted.Billing.KeyLimits, wantLimits) {
		t.Errorf("persisted billing key limits = %#v, want %#v", persisted.Billing.KeyLimits, wantLimits)
	}
}

func cloneStringMap(input map[string]string) map[string]string {
	if input == nil {
		return nil
	}
	cloned := make(map[string]string, len(input))
	for key, value := range input {
		cloned[key] = value
	}
	return cloned
}

func cloneFloatMap(input map[string]float64) map[string]float64 {
	if input == nil {
		return nil
	}
	cloned := make(map[string]float64, len(input))
	for key, value := range input {
		cloned[key] = value
	}
	return cloned
}
