package api

import (
	"bytes"
	"crypto/rand"
	_ "embed"
	"encoding/base64"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

const (
	billingPagePath             = "/billing.html"
	billingPageEmbeddedQuery    = "embedded"
	billingPageEmbeddedValue    = "1"
	billingManagementURL        = "/management.html?cpa-view=billing"
	billingPageNoncePlaceholder = "{{NONCE}}"
	billingManagementModuleID   = "cpa-billing-management-module"
)

//go:embed billing_page.html
var billingPageHTML string

//go:embed billing_management.html
var billingManagementHTML []byte

func injectBillingManagementModule(page []byte) []byte {
	if bytes.Contains(page, []byte(`id="`+billingManagementModuleID+`"`)) {
		return page
	}
	index := bytes.LastIndex(page, []byte("</body>"))
	if index < 0 {
		out := make([]byte, 0, len(page)+len(billingManagementHTML))
		out = append(out, page...)
		return append(out, billingManagementHTML...)
	}
	out := make([]byte, 0, len(page)+len(billingManagementHTML))
	out = append(out, page[:index]...)
	out = append(out, billingManagementHTML...)
	return append(out, page[index:]...)
}

func (s *Server) serveBillingPage(c *gin.Context) {
	cfg := s.cfg
	if cfg == nil || cfg.Home.Enabled || cfg.RemoteManagement.DisableControlPanel {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	if c.Query(billingPageEmbeddedQuery) != billingPageEmbeddedValue {
		c.Redirect(http.StatusTemporaryRedirect, billingManagementURL)
		return
	}

	nonceBytes := make([]byte, 18)
	if _, errRead := rand.Read(nonceBytes); errRead != nil {
		log.WithError(errRead).Error("failed to generate billing page nonce")
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	nonce := base64.RawStdEncoding.EncodeToString(nonceBytes)
	body := strings.ReplaceAll(billingPageHTML, billingPageNoncePlaceholder, nonce)

	c.Header("Cache-Control", "no-store")
	c.Header("Content-Type", "text/html; charset=utf-8")
	c.Header("Content-Security-Policy", "default-src 'none'; base-uri 'none'; connect-src 'self' http: https:; form-action 'self'; frame-ancestors 'self'; img-src 'self' data:; script-src 'nonce-"+nonce+"'; style-src 'nonce-"+nonce+"'")
	c.Header("Referrer-Policy", "no-referrer")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("X-Frame-Options", "SAMEORIGIN")
	if c.Request.Method == http.MethodHead {
		c.Status(http.StatusOK)
		return
	}
	c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(body))
}
