package kube

import (
	"strings"
	"testing"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
)

// TestRestoreOrderPutsTokensLast: Restore re-creates Secrets, which get a
// new creationTimestamp, and the auth layer refuses a token older than its
// owner (docs/m5/security.md S2). The sorted snapshot lists APIToken first,
// so Restore must apply tokens after users and service accounts or a
// restore would invalidate them.
func TestRestoreOrderPutsTokensLast(t *testing.T) {
	scheme, err := internalv1.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	doc := `apiVersion: v1
kind: List
items:
- {apiVersion: kudurru.io/v1alpha1, kind: APIToken, metadata: {name: aaaaaaaaaaaa}, spec: {owner: {kind: User, name: alice, id: "local:alice"}, secretHash: "0000000000000000000000000000000000000000000000000000000000000000"}}
- {apiVersion: kudurru.io/v1alpha1, kind: LocalUser, metadata: {name: alice}, spec: {}}
- {apiVersion: kudurru.io/v1alpha1, kind: APIToken, metadata: {name: bbbbbbbbbbbb}, spec: {owner: {kind: ServiceAccount, name: ci, namespace: dns, id: "sa:dns/ci"}, secretHash: "0000000000000000000000000000000000000000000000000000000000000000"}}
- {apiVersion: kudurru.io/v1alpha1, kind: ServiceAccount, metadata: {name: ci, namespace: dns}, spec: {}}
`
	objs, err := decodeObjects(scheme, strings.NewReader(doc))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, o := range restoreOrder(objs) {
		got = append(got, o.GetObjectKind().GroupVersionKind().Kind+"/"+o.GetName())
	}
	want := "LocalUser/alice ServiceAccount/ci APIToken/aaaaaaaaaaaa APIToken/bbbbbbbbbbbb"
	if strings.Join(got, " ") != want {
		t.Fatalf("restore order %v, want %s", got, want)
	}
}
