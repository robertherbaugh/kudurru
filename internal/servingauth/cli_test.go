package servingauth

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth/clioidc"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

func TestCLIRawProofAndReceiptAcrossActualRestartAndReplica(t *testing.T) {
	for _, mode := range []string{"browser", "device"} {
		t.Run(mode, func(t *testing.T) { cliRawRestartAndReplica(t, mode) })
	}
}
func cliRawRestartAndReplica(t *testing.T, mode string) {
	f := newExternalFixture(t)
	f.claims["aud"] = "cli"
	f.claims["nonce"] = strings.Repeat("A", 43)
	q := clioidc.PrepareRequest{Mode: "browser", Nonce: strings.Repeat("A", 43), RedirectURI: "http://127.0.0.1:8000/callback"}
	if mode == "device" {
		delete(f.claims, "nonce")
		q = clioidc.PrepareRequest{Mode: "device"}
	}
	raw := f.sign(t, f.claims)
	v := f.view(t)
	info, e := v.ProviderInfo("corp")
	if e != nil {
		t.Fatal("provider")
	}
	d := info.Descriptor
	s := clioidc.Source{Provider: d.Ref.ID, UID: d.Ref.UID, Origin: string(d.Ref.Origin), Spec: d.SpecIdentity, Policy: d.PolicyIdentity, Resource: info.ResourceIdentity, Issuer: d.Issuer, ClientID: d.Spec.CLIClientID}
	sealed, _, e := v.SealCLIReceipt(s, q, f.now)
	if e != nil {
		t.Fatal("receipt")
	}
	assert := func(view *View) {
		t.Helper()
		r, e := view.OpenCLIReceipt(sealed, f.now)
		if e != nil {
			t.Fatal("original source correlation")
		}
		for range 2 {
			p, exp, e := view.VerifyCLILogin(t.Context(), Credential{Kind: OIDCJWT, Value: raw}, r, "", f.now)
			if e != nil || p.Provider != "corp" || !exp.After(f.now) {
				t.Fatal("raw current bearer admission")
			}
		}
		if _, _, e := view.VerifyCLILogin(t.Context(), Credential{Kind: Cookie, Value: raw}, r, "", f.now); e == nil {
			t.Fatal("cookie substituted")
		}
		if _, _, e := view.VerifyCLILogin(t.Context(), Credential{Kind: OIDCJWT, Value: raw}, r, "", f.now.Add(5*time.Minute)); e == nil {
			t.Fatal("original proof expiry bypassed")
		}
	}
	assert(v)
	before := f.image(t)
	if f.st.Close() != nil {
		t.Fatal("close fixture")
	}
	f.st = openStore(t, f.path, replication.Standalone)
	f.resources(t)
	assert(f.view(t))
	source := f.image(t)
	peer := openStore(t, filepath.Join(t.TempDir(), "cli-follower.db"), replication.Follower)
	if peer.Enroll(t.Context(), source.Term) != nil {
		t.Fatal("replica enrollment")
	}
	validated, e := peer.ValidateImage(t.Context(), *source)
	if e != nil {
		t.Fatal("replica validation")
	}
	a := peer.Authority()
	if _, e = peer.InstallReplica(t.Context(), validated, replication.InstallOptions{ExpectedTerm: a.Term, ExpectedInstalled: a.Installed}); e != nil {
		t.Fatal("replica install")
	}
	image, e := peer.ReadGeneration(t.Context())
	if e != nil {
		t.Fatal("replica read")
	}
	cut, e := CaptureImage(t.Context(), image, f.registry)
	if e != nil {
		t.Fatal("replica cut")
	}
	replica, e := Prepare(t.Context(), cut, f.deps)
	if e != nil {
		t.Fatal("replica auth")
	}
	defer replica.Release()
	assert(replica)
	after := f.image(t)
	if len(before.Payload.Security) != len(after.Payload.Security) {
		t.Fatal("read-only verification wrote authority")
	}
	changed := s
	changed.Resource = strings.Repeat("f", 64)
	if _, _, e = v.SealCLIReceipt(changed, q, f.now); e == nil {
		t.Fatal("resource substitution")
	}
}
