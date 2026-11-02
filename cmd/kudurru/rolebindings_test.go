package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	publicv1 "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
)

func TestRoleBindingsCLIActualRouterReviewedMutations(t *testing.T) {
	f := newServingHTTPFixture(t, false)
	var mu sync.Mutex
	var requests []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.Method+" "+r.URL.Path)
		mu.Unlock()
		f.server.ServeHTTP(w, r)
	}))
	defer srv.Close()
	execute := func(input string, args ...string) (string, error) {
		t.Helper()
		var out strings.Builder
		cmd := newRoot()
		cmd.SetIn(strings.NewReader(input))
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(append([]string{"--server", srv.URL, "--token", f.token, "-o", "json", "iam", "rolebindings"}, args...))
		err := cmd.ExecuteContext(t.Context())
		return out.String(), err
	}
	count := func() int { mu.Lock(); defer mu.Unlock(); return len(requests) }
	body := roleBindingBody(t, roleBindingManifest("cli-binding", "viewer", "alice"))
	output, err := execute(body, "create", "-f", "-")
	if err != nil {
		t.Fatal("actual-router CLI create refused")
	}
	var created publicv1.RoleBinding
	if json.Unmarshal([]byte(output), &created) != nil || created.UID == "" {
		t.Fatal("CLI create missing metadata")
	}
	localUsersFresh(t, f)
	if _, err := execute("", "list"); err != nil {
		t.Fatal("actual-router CLI list refused")
	}
	output, err = execute("", "get", "apps", "cli-binding")
	if err != nil {
		t.Fatal("actual-router CLI get refused")
	}
	var read publicv1.RoleBinding
	if json.Unmarshal([]byte(output), &read) != nil || read.UID != created.UID || read.ResourceVersion != created.ResourceVersion {
		t.Fatal("CLI exact review changed")
	}
	n := count()
	if _, err := execute(body, "replace", "apps", "cli-binding", "-f", "-"); err == nil {
		t.Fatal("missing explicit review accepted")
	}
	if count() != n {
		t.Fatal("missing review performed implicit I/O")
	}
	n = count()
	if _, err := execute(body, "replace", "apps", "cli-binding", "-f", "-", "--uid", string(read.UID), "--resource-version", "stale"); err == nil {
		t.Fatal("stale review accepted")
	}
	if count() != n+1 {
		t.Fatal("refused replace fetched or retried")
	}
	n = count()
	desired := roleBindingManifest("cli-binding", "viewer", "bob")
	output, err = execute(roleBindingBody(t, desired), "replace", "apps", "cli-binding", "-f", "-", "--uid", string(read.UID), "--resource-version", read.ResourceVersion)
	if err != nil {
		t.Fatal("actual-router reviewed replacement refused")
	}
	var replaced publicv1.RoleBinding
	if json.Unmarshal([]byte(output), &replaced) != nil || replaced.ResourceVersion == read.ResourceVersion || count() != n+1 {
		t.Fatal("replacement review or single request lost")
	}
	localUsersFresh(t, f)
	if _, err := execute("", "delete", "apps", "cli-binding", "--uid", string(replaced.UID), "--resource-version", replaced.ResourceVersion); err != nil {
		t.Fatal("actual-router CLI reviewed delete refused")
	}
	localUsersFresh(t, f)
	if _, err := execute("", "get", "apps", "cli-binding"); err == nil {
		t.Fatal("deleted binding remains readable")
	}
	n = count()
	for _, args := range [][]string{{"delete", "apps", "cli-binding"}, {"get", "apps/escape", "cli-binding"}, {"--transport", "kubernetes", "list"}} {
		if _, err := execute("", args...); err == nil {
			t.Fatal("unsupported CLI input accepted")
		}
	}
	if count() != n {
		t.Fatal("invalid or unsupported command sent I/O")
	}
	if len(localUsersState(t, f.s.opts.Store).GrantFloors) != 1 {
		t.Fatal("CLI writes bypassed security floors")
	}
}

func TestRoleBindingDesiredCLIRejectsDuplicateAndServerMetadata(t *testing.T) {
	for _, body := range []string{
		`{"apiVersion":"kudurru.io/v1alpha1","kind":"RoleBinding","metadata":{"name":"x","namespace":"apps","uid":"invented"},"spec":{"role":"viewer","subjects":[{"kind":"User","name":"alice"}]}}`,
		"apiVersion: kudurru.io/v1alpha1\nkind: RoleBinding\nmetadata:\n  name: x\n  namespace: apps\nspec:\n  role: viewer\n  role: admin\n  subjects: []\n",
	} {
		cmd := newRoleBindingsCmd(&cliOpts{})
		cmd.SetArgs([]string{"create", "-f", "-"})
		cmd.SetIn(strings.NewReader(body))
		cmd.SetOut(new(strings.Builder))
		cmd.SetErr(new(strings.Builder))
		if err := cmd.ExecuteContext(t.Context()); err == nil {
			t.Fatal("unsafe manifest accepted")
		}
	}
	// The review wrapper is separate from the desired manifest.
	if !validRoleBindingReview(api.RoleBindingReview{UID: "exact-uid", ResourceVersion: "opaque-rv"}) {
		t.Fatal("opaque exact review refused")
	}
}
