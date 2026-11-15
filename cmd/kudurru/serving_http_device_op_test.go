package main

import (
	"encoding/json"
	"net/http"
	"time"
)

type servingDeviceGrant struct {
	issuer   string
	expires  time.Time
	decision string
	used     bool
	polls    int
}

func (p *servingBrowserOP) authorizeDevice(w http.ResponseWriter, r *http.Request, issuer string) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if r.Method != "POST" || r.Header.Get("Authorization") != "" || r.ParseForm() != nil || len(r.PostForm) != 2 || r.PostForm.Get("client_id") != "kudurru-cli" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.devices == nil {
		p.devices = map[string]*servingDeviceGrant{}
	}
	expires := int64(30)
	if p.mode == "grant-expired" {
		expires = 1
	}
	code := randomLoginProof()
	p.devices[code] = &servingDeviceGrant{issuer: issuer, expires: time.Now().Add(time.Duration(expires) * time.Second)}
	_ = json.NewEncoder(w).Encode(map[string]any{"device_code": code, "user_code": "ABCD-EFGH", "verification_uri": issuer + "/approve", "expires_in": expires, "interval": 1})
	if p.deviceReady != nil {
		close(p.deviceReady)
		p.deviceReady = nil
	}
}

func (p *servingBrowserOP) approveDevice(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1024)
	if r.Method != "POST" || r.ParseForm() != nil || r.PostForm.Get("user_code") != "ABCD-EFGH" || (r.PostForm.Get("decision") != "approve" && r.PostForm.Get("decision") != "deny") {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, g := range p.devices {
		if !g.used && time.Now().Before(g.expires) {
			g.decision = r.PostForm.Get("decision")
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	w.WriteHeader(http.StatusBadRequest)
}

func (p *servingBrowserOP) exchangeDevice(w http.ResponseWriter, r *http.Request, issuer string) {
	p.requests.Add(1)
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if r.Method != "POST" || r.Header.Get("Authorization") != "" || r.ParseForm() != nil || len(r.PostForm) != 3 || r.PostForm.Get("client_id") != "kudurru-cli" || r.PostForm.Get("grant_type") != deviceGrantType {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	p.mu.Lock()
	g := p.devices[r.PostForm.Get("device_code")]
	p.deviceTimes = append(p.deviceTimes, time.Now())
	mode := p.mode
	var protocolError string
	switch {
	case g == nil || g.used || g.issuer != issuer:
		protocolError = "invalid_grant"
	case !time.Now().Before(g.expires):
		protocolError = "expired_token"
	case g.decision == "deny":
		protocolError = "access_denied"
		g.used = true
	case g.decision == "":
		protocolError = "authorization_pending"
	case mode == "slowdown" && g.polls == 0:
		protocolError = "slow_down"
	default:
		g.used = true
	}
	if g != nil {
		g.polls++
	}
	p.mu.Unlock()
	if protocolError != "" {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": protocolError})
		return
	}
	p.issueCLIToken(w, issuer, "", mode)
}
