package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec // Fixed RFC6238 test profile.
	"encoding/base32"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth/totp"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func servingHTTPFactorCode(t *testing.T, seed *totp.Seed, now time.Time) string {
	t.Helper()
	raw, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(seed.ProvisioningSecret())
	if err != nil {
		t.Fatal("test seed invalid")
	}
	var step [8]byte
	binary.BigEndian.PutUint64(step[:], uint64(now.Unix()/30)) // #nosec G115 -- current positive test time.
	mac := hmac.New(sha1.New, raw)
	_, _ = mac.Write(step[:])
	digest := mac.Sum(nil)
	offset := int(digest[len(digest)-1] & 15)
	return fmt.Sprintf("%06d", (binary.BigEndian.Uint32(digest[offset:offset+4])&0x7fffffff)%1000000)
}
func TestServingHTTPTOTPLoginPasswordConsumeExactlyOnce(t *testing.T) {
	f := newServingHTTPFixture(t, false)
	var seed *totp.Seed
	err := f.s.opts.Store.Command(t.Context(), f.s.opts.Store.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		var u internalv1.LocalUser
		if err := tx.Get(ctx, store.Key{Kind: "LocalUser", Name: "root-command"}, &u); err != nil {
			return err
		}
		var err error
		seed, err = totp.Generate(totp.Account{Name: u.Name, UID: string(u.UID)}, 1)
		if err != nil {
			return err
		}
		keys, err := totp.NewKeyring(bytesOfOne(32), nil)
		if err != nil {
			return err
		}
		u.Spec.TOTPSecret, err = keys.Seal(seed)
		if err != nil {
			return err
		}
		if _, err = tx.Apply(ctx, &u, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
			return err
		}
		security, err := servingHTTPSecurity(ctx, tx)
		if err != nil {
			return err
		}
		a, version, id := seed.Identity()
		security.Epoch++
		security.TOTP = append(security.TOTP, totp.Record{FormatVersion: 1, Account: a, SeedVersion: version, SeedID: id, Enabled: true})
		rows, err := servingauth.EncodeSecurity(security)
		if err != nil {
			return err
		}
		for _, row := range rows {
			if err = tx.PutSection(ctx, "security", row); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	f.s.publisher.Wake()
	awaitServingHTTPView(t, f.s, func(v *servingauth.View) bool {
		objects, e := v.Objects("LocalUser")
		if e != nil {
			return false
		}
		for _, o := range objects {
			if o.(*internalv1.LocalUser).Spec.TOTPSecret != "" {
				return true
			}
		}
		return false
	})
	expectServingHTTPStatus(t, f.call(t, "POST", "/auth/login", `{"username":"root-command","password":"initial-password"}`, "", nil, ""), 401)
	code := servingHTTPFactorCode(t, seed, time.Now())
	//nolint:gosec // G117: intentional test-only login request, never logged.
	body, _ := json.Marshal(api.LoginRequest{Username: "root-command", Password: "initial-password", TOTPCode: code})
	w := f.call(t, "POST", "/auth/login", string(body), "", nil, "")
	expectServingHTTPStatus(t, w, 200)
	cookies := w.Result().Cookies()
	var info api.SessionInfo
	if err = json.Unmarshal(w.Body.Bytes(), &info); err != nil || len(cookies) != 1 {
		t.Fatal("factor login enrollment missing", err)
	}
	// Local admission can reject immediately, and durable replay rejects after
	// the per-second limiter expires. No second password/TOTP verification occurs.
	expectServingHTTPStatus(t, f.call(t, "POST", "/auth/login", string(body), "", nil, ""), 429)
	f.h.factorLimit.Reset("root-command")
	expectServingHTTPStatus(t, f.call(t, "POST", "/auth/login", string(body), "", nil, ""), 401)
	f.h.factorLimit.Reset("root-command")
	next := servingHTTPFactorCode(t, seed, time.Now().Add(30*time.Second))
	change, _ := json.Marshal(api.PasswordChangeRequest{CurrentPassword: "initial-password", NewPassword: "factor-replacement-password", TOTPCode: next})
	changed := f.call(t, "POST", "/auth/password", string(change), "", cookies[0], info.CSRF)
	expectServingHTTPStatus(t, changed, 200)
	image, err := f.s.opts.Store.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	security, err := servingauth.DecodeSecurity(image.Payload.Security)
	if err != nil || len(security.TOTP) != 1 || !security.TOTP[0].HasConsumed {
		t.Fatal("factor replay state missing", err)
	}
	step, err := totp.Match(seed, next, time.Now())
	if err != nil || security.TOTP[0].LastConsumedStep != step {
		t.Fatal("password replacement lost factor consumption", err)
	}
}
func bytesOfOne(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = 1
	}
	return b
}
