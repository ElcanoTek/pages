// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package central

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"

	"github.com/ElcanoTek/pages/internal/auth/wire"
)

// Client is the relying-party side of Auth's application handoff.
type Client struct {
	Config Config
	// Keys is the KeyResolver that also verifies back-channel tokens: one
	// key set for both, so a rotation reaches both at once.
	Keys *KeyResolver
	// HTTP posts to /token (default: no redirects, 10 s timeout).
	HTTP *http.Client
	// Clock is the verification time in Unix seconds (default: wall clock).
	Clock func() int64
}

// NewClient builds a Client. keys is required.
func NewClient(config Config, keys *KeyResolver) (*Client, error) {
	if keys == nil {
		return nil, errNoResolver
	}
	return &Client{
		Config: config,
		Keys:   keys,
		HTTP:   &http.Client{Timeout: HTTPTimeout, CheckRedirect: noRedirects},
		Clock:  now,
	}, nil
}

// CallbackURL is the registered redirect_uri.
func (c *Client) CallbackURL() string { return c.Config.PublicURL + "/auth/callback" }

// AuthorizationURL is Auth's /authorize request for one transaction.
func (c *Client) AuthorizationURL(state, nonce, codeChallenge string) string {
	q := wire.FormEncode(
		[2]string{"response_type", "code"},
		[2]string{"client_id", c.Config.ClientID},
		[2]string{"redirect_uri", c.CallbackURL()},
		[2]string{"scope", "openid email"},
		[2]string{"state", state},
		[2]string{"code_challenge", codeChallenge},
		[2]string{"code_challenge_method", "S256"},
		[2]string{"nonce", nonce},
	)
	return c.Config.Issuer + "/authorize?" + q
}

// LogoutURL is Auth's RP-initiated logout for this application.
func (c *Client) LogoutURL() string {
	return c.Config.Issuer + "/logout?client_id=" + wire.EncodeURIComponent(c.Config.ClientID)
}

// Principal is the identity a code exchange proved.
type Principal struct {
	Subject string
	Email   string
}

// Exchange trades the code for Auth's identity claims.
func (c *Client) Exchange(ctx context.Context, code, verifier, nonce string) (Principal, error) {
	cfg := c.Config
	body := wire.FormEncode(
		[2]string{"grant_type", "authorization_code"},
		[2]string{"code", code},
		[2]string{"redirect_uri", c.CallbackURL()},
		[2]string{"client_id", cfg.ClientID},
		[2]string{"code_verifier", verifier},
	)
	ctx, cancel := context.WithTimeout(ctx, HTTPTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.Issuer+"/token", strings.NewReader(body))
	if err != nil {
		return Principal{}, newErr(nil, "the authentication service is unreachable")
	}
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(cfg.ClientID+":"+cfg.ClientSecret)))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Cache-Control", "no-store")
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: HTTPTimeout, CheckRedirect: noRedirects}
	}
	res, err := hc.Do(req)
	if err != nil {
		return Principal{}, newErr(nil, "the authentication service is unreachable")
	}
	defer res.Body.Close()
	// 400 is invalid_grant: consumed, expired, or superseded by a newer
	// /authorize in another tab. A retry, not an outage.
	if res.StatusCode == http.StatusBadRequest {
		return Principal{}, newErr(ErrCodeExchangeRejected, "the sign-in code was rejected")
	}
	if res.StatusCode != http.StatusOK {
		return Principal{}, newErr(nil, "the authentication service rejected the code exchange")
	}
	raw, err := readCapped(res.Body, MaxTokenResponseBytes)
	if err != nil {
		if errors.Is(err, ErrCentralAuth) {
			return Principal{}, err
		}
		return Principal{}, newErr(nil, "the authentication service is unreachable")
	}
	idToken, _ := parseJSON(raw)["id_token"].(string)
	invalid := newErr(nil, "the authentication response was invalid")
	if idToken == "" {
		return Principal{}, invalid
	}
	clock := c.Clock
	if clock == nil {
		clock = now
	}
	keys := c.Keys.KeysForToken(ctx, idToken)
	claims, err := VerifyIDToken(idToken, IDTokenOptions{Issuer: cfg.Issuer, Audience: cfg.ClientID, Nonce: nonce, Keys: keys, At: clock()})
	if err != nil {
		return Principal{}, invalid
	}
	email, err := NormalizeEmail(claims["email"].(string))
	if err != nil {
		return Principal{}, invalid
	}
	return Principal{Subject: claims["sub"].(string), Email: email}, nil
}
