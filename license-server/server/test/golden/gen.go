//go:build ignore

// gen writes golden.json: license keys signed by internal/license.Sign (the
// Go signer the nodes trust) with a fixed, test-only seed, so the license
// server's TypeScript signer can be checked byte for byte against it
// (server/test/jws.test.ts). The seed is public and its kid, server-test-1,
// must never be added to internal/license/keys.
//
//	go run ./license-server/server/test/golden/gen.go > license-server/server/test/golden/golden.json
package main

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"os"

	"github.com/robertherbaugh/kudurru/internal/license"
)

type vector struct {
	Name   string         `json:"name"`
	Claims license.Claims `json:"claims"`
	Key    string         `json:"key"`
}

func main() {
	seed := sha256.Sum256([]byte("kudurru license-server golden test key 1"))
	priv := ed25519.NewKeyFromSeed(seed[:])
	const kid = "server-test-1"
	base := license.Claims{ID: "lic_AAAAAAAAAAAA", Issuer: license.Issuer, Licensee: "Homelab Hank", Plan: license.PlanPersonal, Nodes: 3, IssuedAt: 1791849600, ExpiresAt: 1823385600}
	cases := []vector{
		{Name: "personal, all features", Claims: base},
		{Name: "business, feature subset", Claims: with(base, func(c *license.Claims) {
			c.ID, c.Licensee, c.Plan, c.Nodes = "lic_b-_9xYz0123", "Example GmbH", license.PlanBusiness, 25
			c.Features = []license.Feature{license.FeatureOIDC, license.FeatureEdge, license.FeatureAudit}
		})},
		{Name: "HTML-safe escaping", Claims: with(base, func(c *license.Claims) {
			c.Licensee = "AT&T <Labs>  line para \"quoted\" back\\slash"
		})},
		{Name: "non-ASCII and emoji", Claims: with(base, func(c *license.Claims) {
			c.Licensee = "Zoë Ångström 東京 🦉"
		})},
		{Name: "control characters", Claims: with(base, func(c *license.Claims) {
			c.Licensee = "tab\there\nnewline \b\f\x01\x1f end"
		})},
		{Name: "with nbf", Claims: with(base, func(c *license.Claims) {
			c.NotBefore = 1791849600
			c.Plan = license.PlanTrial
		})},
		{Name: "enterprise, every feature listed", Claims: with(base, func(c *license.Claims) {
			// This is the original v0.1 signing vector. Keep its claims fixed as
			// the catalog grows; branding-policy and node-oracle tests cover new
			// entitlements without rewriting existing signed compatibility data.
			c.Plan, c.Nodes = license.PlanEnterprise, 100000
			c.Features = []license.Feature{license.FeatureOIDC, license.FeatureEdge, license.FeatureIntegrations,
				license.FeatureRBAC, license.FeatureAudit, license.FeatureLongTerm, license.FeatureRequests}
		})},
	}
	for i := range cases {
		k, err := license.Sign(priv, kid, cases[i].Claims)
		if err != nil {
			panic(err)
		}
		cases[i].Key = k
	}
	out := map[string]any{
		"kid":       kid,
		"seed":      base64.StdEncoding.EncodeToString(seed[:]),
		"publicKey": license.EncodePublicKey(priv.Public().(ed25519.PublicKey)),
		"vectors":   cases,
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(out); err != nil {
		panic(err)
	}
}

func with(c license.Claims, f func(*license.Claims)) license.Claims {
	f(&c)
	return c
}
