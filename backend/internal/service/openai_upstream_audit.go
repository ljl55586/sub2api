package service

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

func withOpenAIUpstreamAudit(req *http.Request, c *gin.Context, account *Account, body []byte) *http.Request {
	if req == nil {
		return req
	}
	meta := UpstreamAuditMeta{
		Provider:  "openai",
		Route:     openAIAuditRoute(c),
		Profile:   HTTPUpstreamProfileOpenAI,
		Transport: "http",
	}
	if account != nil {
		meta.AccountID = account.ID
		meta.AccountType = string(account.Type)
	}
	ctx := WithUpstreamAuditMeta(req.Context(), meta)
	ctx = WithUpstreamAuditBody(ctx, body)
	return req.WithContext(ctx)
}

func openAIAuditRoute(c *gin.Context) string {
	if c == nil || c.Request == nil || c.Request.URL == nil {
		return ""
	}
	return c.Request.URL.Path
}

func openAIWSAuditMeta(c *gin.Context, account *Account) UpstreamAuditMeta {
	meta := UpstreamAuditMeta{
		Provider:  "openai",
		Route:     openAIAuditRoute(c),
		Profile:   HTTPUpstreamProfileOpenAI,
		Transport: "websocket",
	}
	if account != nil {
		meta.AccountID = account.ID
		meta.AccountType = string(account.Type)
		if account.ProxyID != nil && account.Proxy != nil {
			meta.ProxyConfigured = true
		}
	}
	return meta
}
