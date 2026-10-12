package domainlist

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
)

var sharedValidator = sync.OnceValues(validator.New)

type fixture struct {
	t   *testing.T
	st  store.Store
	svc *Service
	now time.Time
}

func meta(name string) metav1.ObjectMeta { return metav1.ObjectMeta{Name: name, Namespace: "default"} }

// newFixture opens a sqlite store with the CRD validator (every write is
// checked against the BlockList schema) holding the default and kids
// groups and policies (API-managed) and a file-managed policy "gitops" for
// the default group.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	v, err := sharedValidator()
	if err != nil {
		t.Fatal(err)
	}
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "state.db"), sqlite.Options{Validator: v, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	f := &fixture{t: t, st: st, now: time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)}
	f.svc = New(Options{Store: st, Now: func() time.Time { return f.now }, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	ctx := context.Background()
	for _, o := range []store.Object{
		&v1alpha1.ClientGroup{ObjectMeta: meta("default"), Spec: v1alpha1.ClientGroupSpec{Clients: []v1alpha1.ClientMatcher{{CIDR: "0.0.0.0/0"}}}},
		&v1alpha1.ClientGroup{ObjectMeta: meta("kids"), Spec: v1alpha1.ClientGroupSpec{Clients: []v1alpha1.ClientMatcher{{CIDR: "10.0.20.0/24"}}}},
		&v1alpha1.ClientGroup{ObjectMeta: meta("lonely"), Spec: v1alpha1.ClientGroupSpec{Clients: []v1alpha1.ClientMatcher{{IP: "10.0.30.1"}}}},
		&v1alpha1.Policy{ObjectMeta: meta("default"), Spec: v1alpha1.PolicySpec{ClientGroupRefs: []string{"default"}}},
		&v1alpha1.Policy{ObjectMeta: meta("kids"), Spec: v1alpha1.PolicySpec{ClientGroupRefs: []string{"kids"}}},
	} {
		if _, err := st.Apply(ctx, o, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
			t.Fatal(err)
		}
	}
	gitops := &v1alpha1.Policy{ObjectMeta: meta("gitops"), Spec: v1alpha1.PolicySpec{ClientGroupRefs: []string{"default"}}}
	if _, err := st.Apply(ctx, gitops, store.ApplyOptions{Manager: "file"}); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) list(name string) *v1alpha1.BlockList {
	f.t.Helper()
	bl := &v1alpha1.BlockList{}
	if err := f.st.Get(context.Background(), store.Key{Kind: "BlockList", Namespace: "default", Name: name}, bl); errors.Is(err, store.ErrNotFound) {
		return nil
	} else if err != nil {
		f.t.Fatal(err)
	}
	return bl
}

func (f *fixture) policy(name string) *v1alpha1.Policy {
	f.t.Helper()
	p := &v1alpha1.Policy{}
	if err := f.st.Get(context.Background(), store.Key{Kind: "Policy", Namespace: "default", Name: name}, p); err != nil {
		f.t.Fatal(err)
	}
	return p
}

func entries(bl *v1alpha1.BlockList) []string {
	var out []string
	for _, src := range bl.Spec.Sources {
		for _, e := range src.Entries {
			s := string(e.Type) + ":" + e.Value
			if !e.IsEnabled() {
				s += ":off"
			}
			out = append(out, s)
		}
	}
	return out
}

func TestNormalize(t *testing.T) {
	ok := []struct {
		t        v1alpha1.EntryType
		in, want string
	}{
		{v1alpha1.EntryExact, " Ads.Example.COM. ", "ads.example.com"},
		{v1alpha1.EntryExact, "_dmarc.example.com", "_dmarc.example.com"},
		{v1alpha1.EntryWildcard, "*.example.com", "*.example.com"},
		{v1alpha1.EntryWildcard, "||Example.com^", "*.example.com"},
		{v1alpha1.EntryWildcard, "example.com", "*.example.com"},
		{v1alpha1.EntryRegex, `/^ads?[0-9]*\./`, `^ads?[0-9]*\.`},
		{v1alpha1.EntryRegex, `(^|\.)doubleclick\.net$`, `(^|\.)doubleclick\.net$`},
	}
	for _, c := range ok {
		got, err := Normalize(c.t, c.in)
		if err != nil || got != c.want {
			t.Errorf("Normalize(%s, %q) = %q, %v; want %q", c.t, c.in, got, err, c.want)
		}
	}
	bad := []struct {
		t  v1alpha1.EntryType
		in string
	}{
		{v1alpha1.EntryExact, "*.example.com"},
		{v1alpha1.EntryExact, "||example.com^"},
		{v1alpha1.EntryExact, "exa mple.com"},
		{v1alpha1.EntryExact, "192.0.2.1"},
		{v1alpha1.EntryExact, ""},
		{v1alpha1.EntryWildcard, "*.*.example.com"},
		{v1alpha1.EntryRegex, "(unclosed"},
		{v1alpha1.EntryRegex, "//"},
	}
	for _, c := range bad {
		if got, err := Normalize(c.t, c.in); !errors.Is(err, ErrInvalid) {
			t.Errorf("Normalize(%s, %q) = %q, %v; want ErrInvalid", c.t, c.in, got, err)
		}
	}
}

func TestListNames(t *testing.T) {
	for name, want := range map[string][2]string{
		"ui-allow": {"allow", ""}, "ui-deny": {"deny", ""}, "ui-allow-kids": {"allow", "kids"}, "ui-deny-a-b": {"deny", "a-b"},
	} {
		a, g, ok := ParseListName(name)
		if !ok || string(a) != want[0] || g != want[1] || ListName(a, g) != name {
			t.Errorf("ParseListName(%q) = %s, %q, %v", name, a, g, ok)
		}
	}
	for _, name := range []string{"ui-allowed", "ui-allow-", "starter"} {
		if _, _, ok := ParseListName(name); ok {
			t.Errorf("ParseListName(%q) ok", name)
		}
	}
	id := ID("default", v1alpha1.EntryRegex, `^a\nb`)
	ns, typ, v, err := ParseID(id)
	if err != nil || ns != "default" || typ != v1alpha1.EntryRegex || v != `^a\nb` {
		t.Errorf("ParseID(ID) = %s %s %s %v", ns, typ, v, err)
	}
	if _, _, _, err := ParseID("!!"); !errors.Is(err, ErrNotFound) {
		t.Errorf("ParseID(garbage) = %v", err)
	}
}

func TestAddEveryone(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	res, err := f.svc.Add(ctx, AddRequest{
		Action: Allow, Namespace: "default", Type: v1alpha1.EntryExact,
		Values: []string{"Good.Example", "*.bad", "good.example"}, Comment: "work", Actor: "user:alice", Origin: v1alpha1.OriginQueryLog,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Errors) != 1 || res.Errors[0].Item != "*.bad" || !slices.Equal(res.New, []string{"good.example"}) {
		t.Fatalf("result %+v", res)
	}
	if want := []string{"default/default", "default/kids"}; !slices.Equal(res.Attached, want) || !slices.Equal(res.Skipped, []string{"default/gitops"}) {
		t.Errorf("attached %v skipped %v", res.Attached, res.Skipped)
	}
	if !slices.Equal(res.Names, []string{"good.example"}) || !slices.Equal(res.IDs, []string{ID("default", v1alpha1.EntryExact, "good.example")}) {
		t.Errorf("names %v ids %v", res.Names, res.IDs)
	}
	bl := f.list(AllowList)
	if bl == nil || bl.Spec.Mode != "allow" || !slices.Equal(entries(bl), []string{"exact:good.example"}) {
		t.Fatalf("ui-allow = %+v", bl)
	}
	e := bl.Spec.Sources[0].Entries[0]
	if bl.Spec.Sources[0].Name != SourceName || e.Comment != "work" || e.CreatedBy != "user:alice" || e.Origin != v1alpha1.OriginQueryLog || e.Created == nil || !e.Created.Time.Equal(f.now) {
		t.Errorf("entry %+v", e)
	}
	if !slices.Contains(f.policy("default").Spec.AllowListRefs, AllowList) || !slices.Contains(f.policy("kids").Spec.AllowListRefs, AllowList) || len(f.policy("gitops").Spec.AllowListRefs) != 0 {
		t.Error("ui-allow not attached to the writable policies only")
	}

	set, err := f.svc.Load(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	got := set.Entries(Allow)
	if len(got) != 1 || !got[0].Everyone || len(got[0].Groups) != 0 || got[0].ReadOnly() != "" || !got[0].Enabled() {
		t.Fatalf("entries %+v", got)
	}
	if len(set.Unattached) != 1 || set.Unattached[0].Policy != "default/gitops" || set.Unattached[0].ManagedBy != "file" {
		t.Errorf("unattached %+v", set.Unattached)
	}

	// Adding again with a comment updates it and adds nothing.
	res, err = f.svc.Add(ctx, AddRequest{Action: Allow, Namespace: "default", Type: v1alpha1.EntryExact, Values: []string{"good.example"}, Comment: "again", Enabled: ptrFalse()})
	if err != nil || len(res.New) != 0 {
		t.Fatalf("re-add: %+v %v", res, err)
	}
	if e := f.list(AllowList).Spec.Sources[0].Entries[0]; e.Comment != "again" || e.IsEnabled() || e.CreatedBy != "user:alice" {
		t.Errorf("re-added entry %+v", e)
	}
}

func TestGroupScopeAndUpdate(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.svc.Add(ctx, AddRequest{Action: Deny, Namespace: "default", Type: v1alpha1.EntryWildcard, Values: []string{"games.example"}, Scope: Scope{Groups: []string{"lonely"}}}); !errors.Is(err, ErrNoPolicy) {
		t.Errorf("group without a policy: %v", err)
	}
	if _, err := f.svc.Add(ctx, AddRequest{Action: Deny, Namespace: "default", Type: v1alpha1.EntryWildcard, Values: []string{"games.example"}, Scope: Scope{Groups: []string{"nope"}}}); !errors.Is(err, ErrInvalid) {
		t.Errorf("unknown group: %v", err)
	}
	res, err := f.svc.Add(ctx, AddRequest{Action: Deny, Namespace: "default", Type: v1alpha1.EntryWildcard, Values: []string{"games.example"}, Scope: Scope{Groups: []string{"default/kids"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Attached, []string{"default/kids"}) || f.list(DenyList) != nil {
		t.Fatalf("group add: %+v", res)
	}
	if bl := f.list("ui-deny-kids"); bl == nil || bl.Spec.Mode != "block" || !slices.Equal(entries(bl), []string{"wildcard:*.games.example"}) {
		t.Fatalf("ui-deny-kids = %+v", bl)
	}
	if !slices.Contains(f.policy("kids").Spec.BlockListRefs, "ui-deny-kids") || slices.Contains(f.policy("default").Spec.BlockListRefs, "ui-deny-kids") {
		t.Error("the group list is not attached to the group's policy only")
	}

	// Switch off and comment.
	off, note := false, "homework first"
	if _, err := f.svc.Update(ctx, Deny, "default", v1alpha1.EntryWildcard, "*.games.example", Patch{Enabled: &off, Comment: &note}); err != nil {
		t.Fatal(err)
	}
	if e := f.list("ui-deny-kids").Spec.Sources[0].Entries[0]; e.IsEnabled() || e.Comment != note || e.Modified == nil {
		t.Errorf("updated entry %+v", e)
	}
	// Move to everyone and rename: the group list goes away and is detached.
	v := "play.example"
	res, err = f.svc.Update(ctx, Deny, "default", v1alpha1.EntryWildcard, "*.games.example", Patch{Value: &v, Scope: &Scope{}})
	if err != nil {
		t.Fatal(err)
	}
	if f.list("ui-deny-kids") != nil || slices.Contains(f.policy("kids").Spec.BlockListRefs, "ui-deny-kids") {
		t.Error("the emptied group list was kept or is still referenced")
	}
	if got := entries(f.list(DenyList)); !slices.Equal(got, []string{"wildcard:*.play.example:off"}) {
		t.Errorf("ui-deny = %v", got)
	}
	if !slices.Equal(res.IDs, []string{ID("default", v1alpha1.EntryWildcard, "*.play.example")}) || !slices.Equal(res.Names, []string{"games.example", "play.example"}) {
		t.Errorf("result %+v", res)
	}
	// Scope both groups: one copy each, read back as one entry.
	if _, err := f.svc.Update(ctx, Deny, "default", v1alpha1.EntryWildcard, "*.play.example", Patch{Scope: &Scope{Groups: []string{"kids", "default"}}}); err != nil {
		t.Fatal(err)
	}
	set, err := f.svc.Load(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	got := set.Entries(Deny)
	if len(got) != 1 || got[0].Everyone || !slices.Equal(got[0].Groups, []string{"default/default", "default/kids"}) || got[0].Enabled() || len(got[0].Copies) != 2 {
		t.Fatalf("entries %+v", got)
	}
	if f.list(DenyList) != nil {
		t.Error("the everyone list was kept after the scope moved to groups")
	}
	if _, err := f.svc.Delete(ctx, Deny, "default", v1alpha1.EntryWildcard, "*.play.example"); err != nil {
		t.Fatal(err)
	}
	if f.list("ui-deny-kids") != nil || f.list("ui-deny-default") != nil {
		t.Error("delete left a list behind")
	}
	if _, err := f.svc.Delete(ctx, Deny, "default", v1alpha1.EntryWildcard, "*.play.example"); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete: %v", err)
	}
}

// TestSwitchedOffSource: adding to a list whose ui source is switched off
// switches the source on and keeps the earlier entries off.
func TestSwitchedOffSource(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	bl := &v1alpha1.BlockList{ObjectMeta: meta(DenyList), Spec: v1alpha1.BlockListSpec{Mode: "block", Sources: []v1alpha1.ListSource{
		{Name: SourceName, Enabled: ptrFalse(), Entries: []v1alpha1.ListEntry{{Value: "old.example"}}},
	}}}
	if _, err := f.st.Apply(ctx, bl, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Add(ctx, AddRequest{Action: Deny, Namespace: "default", Type: v1alpha1.EntryExact, Values: []string{"new.example"}}); err != nil {
		t.Fatal(err)
	}
	got := f.list(DenyList)
	if !got.Spec.Sources[0].IsEnabled() || !slices.Equal(entries(got), []string{"exact:old.example:off", "exact:new.example"}) {
		t.Errorf("list %+v entries %v", got.Spec.Sources[0].Enabled, entries(got))
	}
	// Switching an entry of a switched-off source on works too.
	got.Spec.Sources[0].Enabled = ptrFalse()
	if _, err := f.st.Apply(ctx, got, store.ApplyOptions{Manager: store.ManagerAPI, ExpectedResourceVersion: got.ResourceVersion}); err != nil {
		t.Fatal(err)
	}
	on := true
	if _, err := f.svc.Update(ctx, Deny, "default", v1alpha1.EntryExact, "old.example", Patch{Enabled: &on}); err != nil {
		t.Fatal(err)
	}
	if got := f.list(DenyList); !got.Spec.Sources[0].IsEnabled() || !slices.Equal(entries(got), []string{"exact:new.example:off", "exact:old.example"}) {
		t.Errorf("after switching old.example on: %v", entries(got))
	}
}

func TestReadOnlyList(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	bl := &v1alpha1.BlockList{ObjectMeta: meta(DenyList), Spec: v1alpha1.BlockListSpec{Mode: "block", Sources: []v1alpha1.ListSource{{Name: SourceName, Entries: []v1alpha1.ListEntry{{Value: "x.example"}}}}}}
	if _, err := f.st.Apply(ctx, bl, store.ApplyOptions{Manager: "file"}); err != nil {
		t.Fatal(err)
	}
	set, err := f.svc.Load(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	if e := set.Entries(Deny); len(e) != 1 || e[0].ReadOnly() != "file" {
		t.Fatalf("entries %+v", e)
	}
	off := false
	if _, err := f.svc.Update(ctx, Deny, "default", v1alpha1.EntryExact, "x.example", Patch{Enabled: &off}); !store.IsManagedBy(err) {
		t.Errorf("update of a file-managed entry: %v", err)
	}
	if _, err := f.svc.Add(ctx, AddRequest{Action: Deny, Namespace: "default", Type: v1alpha1.EntryExact, Values: []string{"y.example"}}); !store.IsManagedBy(err) {
		t.Errorf("add to a file-managed list: %v", err)
	}
}

// TestDeleteWithAManagedCopy: deleting an entry that also has a copy in a
// list managed elsewhere is refused before any list is written; before,
// the writable copy was deleted (and its emptied list detached) and only
// then was the managed one refused.
func TestDeleteWithAManagedCopy(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.svc.Add(ctx, AddRequest{Action: Allow, Namespace: "default", Type: v1alpha1.EntryExact, Values: []string{"x.example"}}); err != nil {
		t.Fatal(err)
	}
	kids := &v1alpha1.BlockList{ObjectMeta: meta("ui-allow-kids"), Spec: v1alpha1.BlockListSpec{Mode: "allow", Sources: []v1alpha1.ListSource{{Name: SourceName, Entries: []v1alpha1.ListEntry{{Value: "x.example"}}}}}}
	if _, err := f.st.Apply(ctx, kids, store.ApplyOptions{Manager: "file"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Delete(ctx, Allow, "default", v1alpha1.EntryExact, "x.example"); !store.IsManagedBy(err) {
		t.Fatalf("delete with a file-managed copy: %v", err)
	}
	if bl := f.list(AllowList); bl == nil || !slices.Equal(entries(bl), []string{"exact:x.example"}) {
		t.Fatalf("ui-allow after the refused delete %+v", bl)
	}
	if !slices.Contains(f.policy("default").Spec.AllowListRefs, AllowList) {
		t.Error("ui-allow was detached by the refused delete")
	}
}

// TestMigration: legacy inline lines of ui-allow (the Domains page and
// approvals before structured entries) become entries; lines that are not
// entries stay; an approval's line takes its request as provenance; the
// approval's release still finds it by its old spelling.
func TestMigration(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	legacy := &v1alpha1.BlockList{ObjectMeta: meta(AllowList), Spec: v1alpha1.BlockListSpec{Mode: "allow", Sources: []v1alpha1.ListSource{
		{Name: SourceName, Inline: []string{"a.example", "||b.example^", "*.c.example", "/^d[0-9]+\\.example$/", "@@||e.example^", "0.0.0.0 f.example g.example"}},
	}}}
	group := &v1alpha1.BlockList{ObjectMeta: meta("ui-allow-kids"), Spec: v1alpha1.BlockListSpec{Mode: "allow", Sources: []v1alpha1.ListSource{{Name: SourceName, Inline: []string{"kid.example"}}}}}
	fileList := &v1alpha1.BlockList{ObjectMeta: meta(DenyList), Spec: v1alpha1.BlockListSpec{Mode: "block", Sources: []v1alpha1.ListSource{{Name: SourceName, Inline: []string{"z.example"}}}}}
	for _, o := range []store.Object{legacy, group} {
		if _, err := f.st.Apply(ctx, o, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.st.Apply(ctx, fileList, store.ApplyOptions{Manager: "file"}); err != nil {
		t.Fatal(err)
	}
	req := &v1alpha1.UnblockRequest{ObjectMeta: meta("req-b"), Spec: v1alpha1.UnblockRequestSpec{Domain: "b.example", Client: "10.0.20.5"}}
	if _, err := f.st.Apply(ctx, req, store.ApplyOptions{Manager: "unblock"}); err != nil {
		t.Fatal(err)
	}
	req.Status = v1alpha1.UnblockRequestStatus{State: v1alpha1.UnblockApproved, Allow: &v1alpha1.UnblockAllow{Scope: "everyone", Match: "subdomains", List: "default/ui-allow", Entry: "||b.example^", Added: true}}
	if err := f.st.UpdateStatus(ctx, req); err != nil {
		t.Fatal(err)
	}

	// Read before the migration: both forms.
	set, err := f.svc.Load(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	if n := len(set.Entries(Allow)); n != 5 {
		t.Fatalf("legacy entries read: %d, want 5", n)
	}
	if e := set.Find(Deny, "default", v1alpha1.EntryExact, "z.example"); e == nil || e.ReadOnly() != "file" || !e.Copies[0].Legacy {
		t.Errorf("file-managed legacy entry %+v", e)
	}

	n, err := f.svc.Migrate(ctx)
	if err != nil || n != 2 {
		t.Fatalf("Migrate = %d, %v; want 2 lists", n, err)
	}
	bl := f.list(AllowList)
	if want := []string{"exact:a.example", "wildcard:*.b.example", "wildcard:*.c.example", `regex:^d[0-9]+\.example$`}; !slices.Equal(entries(bl), want) {
		t.Errorf("migrated entries %v, want %v", entries(bl), want)
	}
	if len(bl.Spec.Sources) != 2 || bl.Spec.Sources[1].Name != unconvertedSource || !slices.Equal(bl.Spec.Sources[1].Inline, []string{"@@||e.example^", "0.0.0.0 f.example g.example"}) {
		t.Errorf("unconverted lines: %+v", bl.Spec.Sources)
	}
	for _, e := range bl.Spec.Sources[0].Entries {
		wantOrigin, wantReq := v1alpha1.OriginMigrated, ""
		if e.Value == "*.b.example" {
			wantOrigin, wantReq = v1alpha1.OriginUnblock, "default/req-b"
		}
		if e.Origin != wantOrigin || e.Request != wantReq || e.Created == nil {
			t.Errorf("entry %s: origin %s request %q created %v", e.Value, e.Origin, e.Request, e.Created)
		}
	}
	if got := entries(f.list("ui-allow-kids")); !slices.Equal(got, []string{"exact:kid.example"}) {
		t.Errorf("group list %v", got)
	}
	if fl := f.list(DenyList); len(fl.Spec.Sources[0].Inline) != 1 {
		t.Error("a file-managed list was migrated")
	}
	if n, err := f.svc.Migrate(ctx); err != nil || n != 0 {
		t.Errorf("second Migrate = %d, %v", n, err)
	}
	// The approval releases its entry by the old spelling.
	if err := f.svc.RemoveRule(ctx, "default", AllowList, "||b.example^", ""); err != nil {
		t.Fatal(err)
	}
	if got := entries(f.list(AllowList)); slices.Contains(got, "wildcard:*.b.example") {
		t.Errorf("RemoveRule left the entry: %v", got)
	}
}
