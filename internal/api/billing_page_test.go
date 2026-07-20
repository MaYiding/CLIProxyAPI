package api

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func TestBillingPage(t *testing.T) {
	server := newTestServer(t)

	t.Run("GET serves self-contained dashboard with hardened headers", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, billingPagePath, nil)
		rr := httptest.NewRecorder()
		server.engine.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d body=%s", rr.Code, http.StatusOK, rr.Body.String())
		}
		if got := rr.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
			t.Fatalf("Content-Type = %q, want text/html; charset=utf-8", got)
		}
		for header, want := range map[string]string{
			"Cache-Control":          "no-store",
			"Referrer-Policy":        "no-referrer",
			"X-Content-Type-Options": "nosniff",
			"X-Frame-Options":        "DENY",
		} {
			if got := rr.Header().Get(header); got != want {
				t.Errorf("%s = %q, want %q", header, got, want)
			}
		}

		body := rr.Body.String()
		for _, want := range []string{
			"Per-key Billing",
			`id="management-key"`,
			"/v0/management/billing/usage",
			"Raw client API keys are never returned",
		} {
			if !strings.Contains(body, want) {
				t.Errorf("billing page missing %q", want)
			}
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
	})

	t.Run("HEAD has no body", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodHead, billingPagePath, nil)
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
