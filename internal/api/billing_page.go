package api

import (
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
	billingPageNoncePlaceholder = "{{NONCE}}"
)

//go:embed billing_page.html
var billingPageHTML string

func (s *Server) serveBillingPage(c *gin.Context) {
	cfg := s.cfg
	if cfg == nil || cfg.Home.Enabled || cfg.RemoteManagement.DisableControlPanel {
		c.AbortWithStatus(http.StatusNotFound)
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
	c.Header("Content-Security-Policy", "default-src 'none'; base-uri 'none'; connect-src 'self'; form-action 'self'; frame-ancestors 'none'; img-src 'self' data:; script-src 'nonce-"+nonce+"'; style-src 'nonce-"+nonce+"'")
	c.Header("Referrer-Policy", "no-referrer")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("X-Frame-Options", "DENY")
	if c.Request.Method == http.MethodHead {
		c.Status(http.StatusOK)
		return
	}
	c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(body))
}
