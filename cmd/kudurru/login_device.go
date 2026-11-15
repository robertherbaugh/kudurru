package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/robertherbaugh/kudurru/internal/apiclient"
	"github.com/robertherbaugh/kudurru/internal/auth/clioidc"
	"github.com/robertherbaugh/kudurru/internal/cli"
	"github.com/spf13/cobra"
)

const deviceGrantType = "urn:ietf:params:oauth:grant-type:device_code"

var errDeviceConnectionTimeout = errors.New("device protocol connection timeout")

type publicDeviceGrant struct {
	DeviceCode      string `json:"device_code"`
	Error           string `json:"error"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int64  `json:"expires_in"`
	Interval        *int64 `json:"interval"`
}

func (publicDeviceGrant) String() string     { return "private device authorization response" }
func (p publicDeviceGrant) GoString() string { return p.String() }

func (p publicDeviceGrant) bounds(start, receipt time.Time) (time.Time, time.Duration, error) {
	if p.Error != "" || p.DeviceCode == "" || len(p.DeviceCode) > 4096 || strings.TrimSpace(p.UserCode) == "" || len(p.UserCode) > 128 || !clioidc.HTTPS(p.VerificationURI) || p.ExpiresIn <= 0 || p.ExpiresIn > 86400 {
		return time.Time{}, 0, clioidc.ErrInvalid
	}
	// Only the intentionally displayed user code is printable. Device codes
	// remain transient POST bodies; control characters must never reach a TTY.
	for _, c := range p.UserCode {
		if c < 32 || c > 126 {
			return time.Time{}, 0, clioidc.ErrInvalid
		}
	}
	interval := int64(5) // RFC8628 default, not an advertised capability.
	if p.Interval != nil {
		interval = *p.Interval
	}
	if interval <= 0 || interval > 86400 {
		return time.Time{}, 0, clioidc.ErrInvalid
	}
	deadline := start.Add(time.Duration(p.ExpiresIn) * time.Second)
	if receipt.Before(deadline) {
		deadline = receipt
	}
	return deadline, time.Duration(interval) * time.Second, nil
}

func (o *cliOpts) deviceLogin(cmd *cobra.Command, r cli.Resolved, options loginOIDCOptions) (string, *apiclient.Me, time.Time, error) {
	ctx, cancel := context.WithTimeout(cmd.Context(), clioidc.Lifetime)
	defer cancel()
	c, id, e := o.selectCLILogin(ctx, r, options)
	if e != nil {
		return "", nil, time.Time{}, e
	}
	p, e := c.PrepareCLILogin(ctx, id, clioidc.PrepareRequest{Mode: "device"})
	if e != nil {
		return "", nil, time.Time{}, loginStageError("prepare OIDC login", e)
	}
	started := o.now()
	if !p.ExpiresAt.After(started) || p.ExpiresAt.After(started.Add(clioidc.Lifetime)) || p.Provider != id || p.ClientID == "" || p.Receipt == "" || !clioidc.HTTPS(p.Issuer) || !clioidc.HTTPS(p.DeviceURL) || !clioidc.HTTPS(p.TokenURL) || len(p.Scopes) > 64 {
		return "", nil, time.Time{}, clioidc.ErrInvalid
	}
	ctx, cancelGrant := context.WithDeadline(ctx, p.ExpiresAt)
	defer cancelGrant()
	op, e := newCLIOPClient(options.caFile)
	if e != nil {
		return "", nil, time.Time{}, loginStageError("configure OP TLS", e)
	}
	defer op.CloseIdleConnections()
	var grant publicDeviceGrant
	e = publicLoginPost(ctx, op, p.DeviceURL, url.Values{"client_id": {p.ClientID}, "scope": {strings.Join(p.Scopes, " ")}}, &grant)
	if e != nil {
		return "", nil, time.Time{}, loginStageError("request OIDC device authorization", e)
	}
	deadline, interval, e := grant.bounds(started, p.ExpiresAt)
	if e != nil || o.now().Before(started) || !o.now().Before(deadline) {
		return "", nil, time.Time{}, clioidc.ErrInvalid
	}
	// Never open verification_uri_complete or display device_code. The user
	// enters only the deliberately displayed short code at the provider.
	if _, e = fmt.Fprintf(cmd.OutOrStdout(), "Provider %s, public client %q.\nOpen %q and enter code %s before %s.\nApprove only a request you initiated; confirm the provider, client and code on the approval page.\n", id, p.ClientID, grant.VerificationURI, grant.UserCode, deadline.UTC().Format(time.RFC3339)); e != nil {
		return "", nil, time.Time{}, clioidc.ErrInvalid
	}
	tokens, e := pollDeviceLogin(ctx, op, p, grant.DeviceCode, deadline, interval, o.now)
	if e != nil {
		return "", nil, time.Time{}, loginStageError("poll OIDC device authorization", e)
	}
	return o.completeCLILogin(ctx, c, id, p, tokens)
}

// Every POST is a distinct RFC8628 poll. Refusals never cause a general retry,
// fresh grant, provider fallback or renewed receipt. Timeouts may only reduce
// polling frequency while retaining the original code and absolute deadline.
func pollDeviceLogin(ctx context.Context, op *http.Client, p clioidc.Preparation, code string, deadline time.Time, interval time.Duration, now func() time.Time) (publicLoginTokens, error) {
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	last := now()
	if interval < time.Second || interval > 24*time.Hour || code == "" || len(code) > 4096 || deadline.After(last.Add(clioidc.Lifetime)) {
		return publicLoginTokens{}, clioidc.ErrInvalid
	}
	// At least one second between polls and the ten-minute original receipt
	// bound already cap this count. The explicit cap is a second allocation/
	// work bound, never permission to poll sooner or extend the deadline.
	for range 600 {
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return publicLoginTokens{}, ctx.Err()
		case <-timer.C:
		}
		observed := now()
		if ctx.Err() != nil {
			return publicLoginTokens{}, ctx.Err()
		}
		if observed.Before(last) || !observed.Before(deadline) {
			return publicLoginTokens{}, clioidc.ErrInvalid
		}
		last = observed
		status, b, e := publicLoginResponse(ctx, op, p.TokenURL, url.Values{"grant_type": {deviceGrantType}, "client_id": {p.ClientID}, "device_code": {code}})
		current := now()
		if ctx.Err() != nil {
			clear(b)
			return publicLoginTokens{}, ctx.Err()
		}
		if current.Before(last) || !current.Before(deadline) {
			clear(b)
			return publicLoginTokens{}, clioidc.ErrInvalid
		}
		last = current
		if errors.Is(e, errDeviceConnectionTimeout) {
			interval *= 2
			continue
		}
		if e != nil {
			clear(b)
			return publicLoginTokens{}, e
		}
		var tokens publicLoginTokens
		e = decodePublicLoginJSON(b, &tokens)
		clear(b)
		if e != nil {
			return publicLoginTokens{}, clioidc.ErrInvalid
		}
		if status == http.StatusOK && tokens.Error == "" && tokens.IDToken != "" {
			return tokens, nil
		}
		if status != http.StatusBadRequest || tokens.IDToken != "" || tokens.AccessToken != "" {
			return publicLoginTokens{}, clioidc.ErrInvalid
		}
		switch tokens.Error {
		case "authorization_pending":
		case "slow_down":
			interval += 5 * time.Second
		default:
			return publicLoginTokens{}, clioidc.ErrInvalid
		}
	}
	return publicLoginTokens{}, clioidc.ErrInvalid
}
