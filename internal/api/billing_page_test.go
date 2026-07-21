package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
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
			"Per-key Billing",
			"cpa-billing-context",
			"/billing/usage",
			"/billing/settings",
			`id="settings-default-price"`,
			`id="settings-keys-body"`,
			"Raw client API keys are never returned",
		} {
			if !strings.Contains(body, want) {
				t.Errorf("billing page missing %q", want)
			}
		}
		if strings.Contains(body, `id="management-key"`) || strings.Contains(body, `id="login-form"`) {
			t.Fatal("embedded billing page contains a second management login")
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
		"/billing.html?embedded=1",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("management page missing %q: %s", want, body)
		}
	}
	if strings.Contains(body, `href="/billing.html"`) {
		t.Fatal("management page still contains the legacy standalone billing link")
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
