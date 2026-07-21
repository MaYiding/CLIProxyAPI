package api

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/billing"
)

const billingLimitExceededCode = "billing_limit_exceeded"

func billingLimitMiddleware(manager *billing.Manager) gin.HandlerFunc {
	return func(c *gin.Context) {
		if manager == nil {
			c.Next()
			return
		}

		rawKey := strings.TrimSpace(c.GetString("userApiKey"))
		if rawKey == "" {
			c.Next()
			return
		}
		status := manager.LimitStatus(rawKey)
		if status.Allowed {
			c.Next()
			return
		}

		c.Header("X-Billing-Currency", status.Currency)
		c.Header("X-Billing-Limit", status.Limit)
		c.Header("X-Billing-Spent", status.Spent)
		c.Header("X-Billing-Remaining", status.Remaining)
		c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
			"error": gin.H{
				"message": "API key billing limit reached",
				"type":    "insufficient_quota",
				"code":    billingLimitExceededCode,
			},
			"billing": gin.H{
				"currency":  status.Currency,
				"limit":     status.Limit,
				"spent":     status.Spent,
				"remaining": status.Remaining,
			},
		})
	}
}
