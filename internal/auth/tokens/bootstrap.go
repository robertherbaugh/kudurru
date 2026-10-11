package tokens

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// Environment variables of the bootstrap token (§8.7).
const (
	EnvBootstrapToken     = "KUDURRU_BOOTSTRAP_TOKEN"
	EnvBootstrapTokenFile = "KUDURRU_BOOTSTRAP_TOKEN_FILE"
)

// BootstrapAccount is the service account that owns the bootstrap token.
const BootstrapAccount = "bootstrap"

// Bootstrap installs KUDURRU_BOOTSTRAP_TOKEN[_FILE] (Helm post-install
// jobs, Terraform) as a token of the builtin-admin service account
// <namespace>/bootstrap; only its hash is stored. It is idempotent across
// restarts and replicas, and replaces the stored hash when the variable
// changes for the same token id. The value must be a wire-format token
// (kdr_<12>_<43>; `kudurru token generate` prints one). Without the
// variables it does nothing and returns false.
func (t *Tokens) Bootstrap(ctx context.Context, getenv func(string) string) (bool, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	wire := getenv(EnvBootstrapToken)
	source := EnvBootstrapToken
	if f := getenv(EnvBootstrapTokenFile); f != "" && wire == "" {
		b, err := os.ReadFile(f) //nolint:gosec // G304: the operator names the file
		if err != nil {
			return false, fmt.Errorf("tokens: %s: %w", EnvBootstrapTokenFile, err)
		}
		wire, source = strings.TrimSpace(string(b)), EnvBootstrapTokenFile
	}
	if wire == "" {
		return false, nil
	}
	id, ok := Parse(wire)
	if !ok {
		return false, fmt.Errorf("tokens: %s is not a kudurru token (want kdr_<12 chars [a-z0-9]>_<43 chars base64url>; `kudurru token generate` prints one)", source)
	}
	ns := t.opts.Namespace
	if _, err := t.EnsureServiceAccount(ctx, ns, BootstrapAccount, "Bootstrap service account ("+EnvBootstrapToken+")", true); err != nil {
		return false, fmt.Errorf("tokens: bootstrap service account: %w", err)
	}
	want := internalv1.APITokenSpec{
		Owner:       internalv1.TokenOwner{Kind: internalv1.OwnerServiceAccount, Name: BootstrapAccount, Namespace: ns, ID: ServiceAccountID(ns, BootstrapAccount)},
		Description: "bootstrap token (" + source + ")",
		SecretHash:  Hash(wire),
	}
	var cur internalv1.APIToken
	err := t.st.Get(ctx, store.Key{Kind: internalv1.KindAPIToken, Name: id}, &cur)
	switch {
	case err == nil && cur.Spec.SecretHash == want.SecretHash && cur.Spec.Owner == want.Owner:
		t.toks.Upsert(&cur)
		return true, nil
	case err != nil && !errors.Is(err, store.ErrNotFound):
		return false, err
	}
	tok := &internalv1.APIToken{ObjectMeta: metav1.ObjectMeta{Name: id}, Spec: want}
	if err := tok.Validate(); err != nil {
		return false, err
	}
	out, err := t.st.Apply(ctx, tok, store.ApplyOptions{Manager: store.ManagerSystem, Force: true})
	if err != nil {
		return false, fmt.Errorf("tokens: bootstrap token: %w", err)
	}
	t.toks.Upsert(out.(*internalv1.APIToken))
	t.log.InfoContext(ctx, "bootstrap token installed", "token", id, "serviceAccount", ns+"/"+BootstrapAccount)
	return true, nil
}
