package iamlegacy

import (
	"strings"
	"testing"

	publicv1 "github.com/robertherbaugh/kudurru/api/v1alpha1"
)

func TestConvertCredentialReferencesWithoutCapabilities(t *testing.T) {
	for _, tc := range []struct{ old, wantFile, wantSecret string }{
		{`"clientSecretFile":"/host/private"`, "/host/private", ""},
		{`"clientSecretRef":{"name":"oidc","key":"token"}`, "", "oidc"},
		{`"clientSecretRef":{"file":"/host/private"}`, "/host/private", ""},
	} {
		raw := []byte(`{"apiVersion":"kudurru.io/v1alpha1","kind":"IdentityProvider","metadata":{"name":"corp","uid":"old-uid","resourceVersion":"opaque-rv","generation":7,"annotations":{"kudurru.io/managed-by":"file:iam.yaml"}},"spec":{"issuer":"https://sso.example","clientID":"test",` + tc.old + `},"status":{"conditions":[{"type":"Ready","status":"False","reason":"Offline","message":"waiting","lastTransitionTime":"2026-10-14T00:00:00Z"}]}}`)
		obj, err := Convert("IdentityProvider", raw)
		if err != nil {
			t.Fatal(err)
		}
		idp := obj.(*publicv1.IdentityProvider)
		if idp.UID != "old-uid" || idp.ResourceVersion != "opaque-rv" || idp.Generation != 7 || idp.Annotations["kudurru.io/managed-by"] != "file:iam.yaml" || len(idp.Status.Conditions) != 1 {
			t.Fatal("conversion changed identity, ownership or status")
		}
		ref := idp.Spec.ClientSecretRef
		if ref.File != tc.wantFile || tc.wantSecret != "" && (ref.SecretRef == nil || ref.SecretRef.Name != tc.wantSecret) {
			t.Fatal("wrong credential reference conversion")
		}
	}
}

func TestConvertRejectsAmbiguityUnknownFieldsAndUnboundedInput(t *testing.T) {
	for _, spec := range []string{
		`"clientSecretFile":"private-value","clientSecretRef":{"name":"oidc","key":"key"}`,
		`"clientSecretFile":"private-value","clientSecretFile":"/second"`,
		`"clientSecretFile":"/private","clientSecret":"private-value"`,
		`"clientSecretRef":{"name":"oidc","key":"key","namespace":"other"}`,
		`"clientSecretRef":{"file":"/private","env":"TOKEN"}`,
	} {
		raw := []byte(`{"metadata":{"name":"corp"},"spec":{"issuer":"https://sso.example","clientID":"test",` + spec + `}}`)
		if _, err := Convert("IdentityProvider", raw); err == nil || strings.Contains(err.Error(), "private-value") {
			t.Fatal("bad conversion accepted or leaked a value")
		}
	}
	for _, raw := range [][]byte{[]byte(strings.Repeat(" ", MaxDocumentBytes+1)), []byte(strings.Repeat("[", 65) + "0" + strings.Repeat("]", 65)), []byte(`{} {}`)} {
		if _, err := Convert("IdentityProvider", raw); err == nil {
			t.Fatal("unbounded or trailing document accepted")
		}
	}
	if _, err := Convert("ServiceAccount", []byte(`{"metadata":{"name":"admin","namespace":"dns"},"spec":{"builtinAdmin":true}}`)); err == nil {
		t.Fatal("private authority discarded before migration")
	}
}
