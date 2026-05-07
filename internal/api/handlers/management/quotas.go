package management

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/quota"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// quotaAccount is the per-credential payload returned by GetQuotas.
type quotaAccount struct {
	ID             string          `json:"id"`
	Provider       string          `json:"provider"`
	Label          string          `json:"label,omitempty"`
	Email          string          `json:"email,omitempty"`
	Status         coreauth.Status `json:"status"`
	StatusMessage  string          `json:"status_message,omitempty"`
	Disabled       bool            `json:"disabled"`
	Unavailable    bool            `json:"unavailable"`
	NextRetryAfter *time.Time      `json:"next_retry_after,omitempty"`
	NextRecoverAt  *time.Time      `json:"next_recover_at,omitempty"`
	Exceeded       bool            `json:"exceeded"`
	Reason         string          `json:"reason,omitempty"`
	Samples        []quota.Sample  `json:"samples"`
	ObservedAt     *time.Time      `json:"observed_at,omitempty"`
}

// GetQuotas returns the live quota snapshot for every authenticated account.
//
// The response merges three sources:
//  1. authManager.List() — every credential the gateway knows about
//  2. quota.Default — most recent rate-limit headers parsed by executors
//  3. auth.QuotaState — existing 429-driven backoff bookkeeping
//
// Accounts with no quota observations yet are still listed (with empty
// `samples`) so the UI can show "no data yet" instead of dropping them.
func (h *Handler) GetQuotas(c *gin.Context) {
	if h == nil || h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "auth manager unavailable"})
		return
	}
	auths := h.authManager.List()
	out := make([]quotaAccount, 0, len(auths))
	for _, a := range auths {
		if a == nil {
			continue
		}
		out = append(out, buildQuotaAccount(a))
	}
	c.JSON(http.StatusOK, gin.H{
		"fetched_at": time.Now().UTC(),
		"accounts":   out,
	})
}

// GetAccountQuotas returns the quota snapshot for a single account by ID.
func (h *Handler) GetAccountQuotas(c *gin.Context) {
	if h == nil || h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "auth manager unavailable"})
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing account id"})
		return
	}
	auth, ok := h.authManager.GetByID(id)
	if !ok || auth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "account not found"})
		return
	}
	c.JSON(http.StatusOK, buildQuotaAccount(auth))
}

// buildQuotaAccount assembles a single account view from the auth record
// plus the quota store snapshot.
func buildQuotaAccount(a *coreauth.Auth) quotaAccount {
	acc := quotaAccount{
		ID:            a.ID,
		Provider:      strings.TrimSpace(a.Provider),
		Label:         a.Label,
		Status:        a.Status,
		StatusMessage: a.StatusMessage,
		Disabled:      a.Disabled,
		Unavailable:   a.Unavailable,
		Exceeded:      a.Quota.Exceeded,
		Reason:        a.Quota.Reason,
		Samples:       []quota.Sample{},
	}
	if email := authEmail(a); email != "" {
		acc.Email = email
	}
	if !a.NextRetryAfter.IsZero() {
		t := a.NextRetryAfter
		acc.NextRetryAfter = &t
	}
	if !a.Quota.NextRecoverAt.IsZero() {
		t := a.Quota.NextRecoverAt
		acc.NextRecoverAt = &t
	}
	if snap, ok := quota.Default.Get(a.ID); ok {
		acc.Samples = snap.Samples
		t := snap.ObservedAt
		acc.ObservedAt = &t
	}
	return acc
}
