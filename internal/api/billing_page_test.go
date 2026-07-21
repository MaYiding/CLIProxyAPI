package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/billing"
)

func TestBillingPage(t *testing.T) {
	server := newTestServer(t)

	t.Run("legacy GET redirects into management", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, billingPagePath, nil)
		rr := httptest.NewRecorder()
		server.engine.ServeHTTP(rr, req)

		if rr.Code != http.StatusTemporaryRedirect {
			t.Fatalf("status = %d, want %d body=%s", rr.Code, http.StatusTemporaryRedirect, rr.Body.String())
		}
		if got := rr.Header().Get("Location"); got != billingManagementURL {
			t.Fatalf("Location = %q, want %q", got, billingManagementURL)
		}
	})

	t.Run("embedded GET serves session-aware dashboard with hardened headers", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, billingPagePath+"?"+billingPageEmbeddedQuery+"="+billingPageEmbeddedValue, nil)
		rr := httptest.NewRecorder()
		server.engine.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d body=%s", rr.Code, http.StatusOK, rr.Body.String())
		}
		for header, want := range map[string]string{
			"Cache-Control":          "no-store",
			"Referrer-Policy":        "no-referrer",
			"X-Content-Type-Options": "nosniff",
			"X-Frame-Options":        "SAMEORIGIN",
		} {
			if got := rr.Header().Get(header); got != want {
				t.Errorf("%s = %q, want %q", header, got, want)
			}
		}

		body := rr.Body.String()
		for _, want := range []string{
			"Per-key billing",
			"分 KEY 计费",
			"分 KEY 計費",
			"Тарификация по ключам",
			"cpa-billing-context",
			"/billing/usage",
			"/billing/settings",
			"/billing/keys/",
			`id="settings-default-price"`,
			`id="key-body"`,
			`data-i18n="tabKeys"`,
			"key_preview",
			"key_truncated",
			"revealIcon",
			"key-value.revealed",
			`classList.add("revealed")`,
			"validateName",
			"keyDisplayText(event.key_id",
			"translateDynamicControls",
			"state.overview = results[0]",
			"state.overview = overview",
			"Short KEYs are shown in full",
		} {
			if !strings.Contains(body, want) {
				t.Errorf("billing page missing %q", want)
			}
		}
		if strings.Contains(body, `id="management-key"`) || strings.Contains(body, `id="login-form"`) {
			t.Fatal("embedded billing page contains a second management login")
		}
		if strings.Contains(body, "if (state.settings) renderSettings(state.settings)") {
			t.Fatal("language changes still reset unsaved billing form drafts")
		}
		if strings.Contains(body, billingPageNoncePlaceholder) {
			t.Fatal("billing page still contains nonce placeholder")
		}

		csp := rr.Header().Get("Content-Security-Policy")
		match := regexp.MustCompile(`script-src 'nonce-([^']+)'`).FindStringSubmatch(csp)
		if len(match) != 2 {
			t.Fatalf("CSP missing script nonce: %q", csp)
		}
		if !strings.Contains(body, `nonce="`+match[1]+`"`) {
			t.Fatal("CSP nonce does not match embedded page nonce")
		}
		if !strings.Contains(csp, "frame-ancestors 'self'") || strings.Contains(csp, "frame-ancestors 'none'") {
			t.Fatalf("CSP does not allow only same-origin management embedding: %q", csp)
		}
	})

	t.Run("embedded HEAD has no body", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodHead, billingPagePath+"?"+billingPageEmbeddedQuery+"="+billingPageEmbeddedValue, nil)
		rr := httptest.NewRecorder()
		server.engine.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
		}
		if rr.Body.Len() != 0 {
			t.Fatalf("body length = %d, want 0", rr.Body.Len())
		}
		if got := rr.Header().Get("Cache-Control"); got != "no-store" {
			t.Fatalf("Cache-Control = %q, want no-store", got)
		}
		if got := rr.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
			t.Fatalf("Content-Type = %q, want text/html; charset=utf-8", got)
		}
	})
}

func TestBillingKeyRevealRouteUsesExistingManagementAuthentication(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "test-management-key")
	server := newTestServer(t)
	rawKey := "sk-1234567890-abcdefghijklmnopqrstuvwxyz"
	server.cfg.APIKeys = []string{rawKey}
	keyID, _ := billing.IdentifyKey(rawKey)
	target := "/v0/management/billing/keys/" + keyID + "/reveal"

	unauthenticated := httptest.NewRecorder()
	server.engine.ServeHTTP(unauthenticated, httptest.NewRequest(http.MethodPost, target, nil))
	if unauthenticated.Code != http.StatusUnauthorized || strings.Contains(unauthenticated.Body.String(), rawKey) {
		t.Fatalf("unauthenticated response = %d %s", unauthenticated.Code, unauthenticated.Body.String())
	}

	request := httptest.NewRequest(http.MethodPost, target, nil)
	request.Header.Set("Authorization", "Bearer test-management-key")
	recorder := httptest.NewRecorder()
	server.engine.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("authenticated response = %d %s", recorder.Code, recorder.Body.String())
	}
	var response map[string]string
	if errUnmarshal := json.Unmarshal(recorder.Body.Bytes(), &response); errUnmarshal != nil {
		t.Fatalf("Unmarshal() error = %v", errUnmarshal)
	}
	if response["key"] != rawKey || recorder.Header().Get("Cache-Control") != "no-store, private, max-age=0" {
		t.Fatalf("authenticated response = %#v headers=%#v", response, recorder.Header())
	}
}

func TestManagementPageIncludesDiscoverableBillingEntry(t *testing.T) {
	staticDir := t.TempDir()
	managementPage := []byte("<!doctype html><html><body><main>management app</main></body></html>")
	if errWrite := os.WriteFile(filepath.Join(staticDir, "management.html"), managementPage, 0o600); errWrite != nil {
		t.Fatalf("WriteFile() error = %v", errWrite)
	}
	t.Setenv("MANAGEMENT_STATIC_PATH", staticDir)
	server := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/management.html", nil)
	recorder := httptest.NewRecorder()
	server.engine.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	body := recorder.Body.String()
	for _, want := range []string{
		"management app",
		`id="cpa-billing-management-module"`,
		`id="cpa-billing-management-script"`,
		"cpa-billing-nav",
		"cpa-view",
		"captureSession",
		"/billing.html?embedded=1&lang=",
		"lastContextSignature",
		"JSON.stringify({ session, theme })",
		"history.replaceState",
		"frameMissing",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("management page missing %q: %s", want, body)
		}
	}
	if strings.Contains(body, `href="/billing.html"`) {
		t.Fatal("management page still contains the legacy standalone billing link")
	}
	if strings.Contains(body, "history.pushState") {
		t.Fatal("billing navigation still adds duplicate browser history entries")
	}
	if strings.Contains(body, "new MutationObserver(queueReconcile)") {
		t.Fatal("billing integration still observes every mutation with an unconditional reconcile loop")
	}
	if count := strings.Count(body, `id="cpa-billing-management-module"`); count != 1 {
		t.Fatalf("billing module count = %d, want 1", count)
	}
}

func TestInjectBillingManagementModuleIsIdempotent(t *testing.T) {
	page := []byte("<html><body>app</body></html>")
	once := injectBillingManagementModule(page)
	twice := injectBillingManagementModule(once)
	if string(once) != string(twice) {
		t.Fatal("injectBillingManagementModule() duplicated or changed an existing module")
	}
	if !strings.Contains(string(once), `id="cpa-billing-management-module"`) || !strings.Contains(string(once), `</body>`) {
		t.Fatalf("injected page = %s", once)
	}
	if strings.Index(string(once), `id="cpa-billing-management-module"`) > strings.Index(string(once), `</body>`) {
		t.Fatal("billing module was injected after the closing body tag")
	}
}

func TestBillingPageUnavailableWithHiddenManagement(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*Server)
	}{
		{
			name: "home runtime",
			configure: func(server *Server) {
				server.cfg.Home.Enabled = true
			},
		},
		{
			name: "control panel disabled",
			configure: func(server *Server) {
				server.cfg.RemoteManagement.DisableControlPanel = true
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := newTestServer(t)
			test.configure(server)
			req := httptest.NewRequest(http.MethodGet, billingPagePath, nil)
			rr := httptest.NewRecorder()
			server.engine.ServeHTTP(rr, req)

			if rr.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want %d body=%s", rr.Code, http.StatusNotFound, rr.Body.String())
			}
		})
	}
}
