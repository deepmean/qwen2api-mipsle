package main

// Account pool and token lifecycle.
//
// Ported from Qwen2API src/utils/account.js + src/utils/account-rotator.js,
// reduced to what the core chat path uses: round-robin selection plus lazy
// sign-in when a token is missing or near expiry. Upstream additionally runs a
// 6-hour background refresh and per-day statistics; neither is needed to serve
// requests, so both are dropped to save RAM.

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"sync"
	"time"
)

// tokenRefreshSkew re-signs in slightly before the JWT actually expires, so a
// request never starts with a token that dies mid-flight.
const tokenRefreshSkew = 5 * time.Minute

var errNoAccounts = errors.New("no accounts configured")

// qwenClient wraps the HTTP client with the upstream base URL.
type qwenClient struct {
	hc *httpClient
}

// pool hands out accounts round-robin and keeps their tokens fresh.
type pool struct {
	mu       sync.Mutex
	accounts []*account
	next     int
	client   *qwenClient
}

func newPool(accounts []*account, client *qwenClient) *pool {
	return &pool{accounts: accounts, client: client}
}

func (p *pool) empty() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.accounts) == 0
}

func (p *pool) size() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.accounts)
}

// acquire returns the next account with a usable token, signing in if needed.
// The account is returned unlocked; token fields are only mutated here while
// holding p.mu, so readers must go through acquire.
func (p *pool) acquire(ctx context.Context) (*account, error) {
	p.mu.Lock()
	if len(p.accounts) == 0 {
		p.mu.Unlock()
		return nil, errNoAccounts
	}
	acct := p.accounts[p.next]
	p.next = (p.next + 1) % len(p.accounts)
	p.mu.Unlock()

	if err := p.ensureToken(ctx, acct); err != nil {
		return nil, err
	}
	return acct, nil
}

// ensureToken signs in when the stored token is absent or about to expire.
func (p *pool) ensureToken(ctx context.Context, acct *account) error {
	p.mu.Lock()
	token := acct.token
	expires := acct.expires
	p.mu.Unlock()

	if token != "" && (expires == 0 || time.Now().Add(tokenRefreshSkew).Unix() < expires) {
		return nil
	}

	signCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()

	token, expires, err := p.client.signIn(signCtx, acct)
	if err != nil {
		return err
	}

	p.mu.Lock()
	acct.token = token
	acct.expires = expires
	p.mu.Unlock()
	return nil
}

// invalidate drops the cached token so the next acquire re-signs in. Called when
// upstream answers 401/403, which means the JWT was revoked server-side.
func (p *pool) invalidate(acct *account) {
	p.mu.Lock()
	acct.token = ""
	acct.expires = 0
	p.mu.Unlock()
}

// health reports per-account status for /health. It performs no network calls.
func (p *pool) health() map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]any, len(p.accounts))
	now := time.Now().Unix()
	for _, a := range p.accounts {
		entry := map[string]any{"signed_in": a.token != ""}
		if a.expires > 0 {
			entry["expires_in_sec"] = a.expires - now
		}
		out[a.Email] = entry
	}
	return out
}

// base64URLDecode decodes JWT segments, tolerating missing padding.
func base64URLDecode(s string) ([]byte, error) {
	if pad := len(s) % 4; pad != 0 {
		s += strings.Repeat("=", 4-pad)
	}
	return base64.URLEncoding.DecodeString(s)
}
