package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/cache"
	"github.com/robertherbaugh/kudurru/internal/domainlist"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func domainCommandHarness(t *testing.T, authorize func([]rbac.Attributes) error) (*harness, *sqlite.Store) {
	t.Helper()
	h, st := objectCommandHarness(t, nil)
	h.srv.opts.DomainCommand = func(ctx context.Context, target DomainCommandTarget, prepare DomainCommandPrepare) error {
		return st.Command(ctx, st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
			plan, err := prepare(ctx, tx)
			if plan != nil {
				defer plan.Release()
			}
			if err != nil {
				return err
			}
			if !plan.PreparedFor(tx, target) {
				return errUnavailable("bad domain plan")
			}
			if authorize != nil {
				if err := authorize(plan.Requirements()); err != nil {
					return err
				}
			}
			return plan.Apply(ctx, tx)
		})
	}
	return h, st
}

func domainListAt(t *testing.T, st *sqlite.Store, name string) *public.BlockList {
	t.Helper()
	var list public.BlockList
	if err := st.Get(t.Context(), store.Key{Kind: "BlockList", Namespace: "default", Name: name}, &list); err != nil {
		t.Fatal(err)
	}
	return &list
}

func TestDomainCommandCurrentInventoryLifecycle(t *testing.T) {
	h, st := domainCommandHarness(t, func(attrs []rbac.Attributes) error {
		if len(attrs) != 1 || attrs[0] != (rbac.Attributes{Resource: "blocklists", Verb: "write", Scope: clusterScope}) {
			t.Fatal("changed implicit permission contract", attrs)
		}
		return nil
	})
	zoneSeed(t, st, store.ManagerBootstrap, &public.Policy{ObjectMeta: objectMeta("default", "default")})
	zoneSeed(t, st, store.ManagerAPI, &public.ClientGroup{ObjectMeta: objectMeta("default", "kids"), Spec: public.ClientGroupSpec{Clients: []public.ClientMatcher{{CIDR: "10.2.0.0/16"}}}}, &public.Policy{ObjectMeta: objectMeta("default", "kids"), Spec: public.PolicySpec{ClientGroupRefs: []string{"kids"}}})
	zoneSeed(t, st, "file:objects.d/policy.yaml", &public.Policy{ObjectMeta: objectMeta("default", "file")})
	// The unrelated legacy Store intentionally lies about group/policy inventory.
	h.seed("file:wrong.yaml", &public.ClientGroup{ObjectMeta: objectMeta("default", "kids")})
	var out DomainWriteResult
	before, _ := st.ReadGeneration(t.Context())
	h.do(http.MethodPost, "/api/v1/allowlist", "operator", map[string]any{"values": []string{"Ads.Example.", "not a domain"}, "comment": "first"}).want(http.StatusOK).json(&out)
	after, _ := st.ReadGeneration(t.Context())
	if after.Revision != before.Revision+1 || len(out.Items) != 1 || out.Items[0].Value != "ads.example" || len(out.Processed.Errors) != 1 || !slices.Equal(out.Skipped, []string{"default/file"}) || !slices.Equal(out.Attached, []string{"default/default", "default/kids"}) {
		t.Fatalf("bad atomic add: %+v", out)
	}
	id := out.Items[0].ID
	h.do(http.MethodPost, "/api/v1/allowlist", "operator", map[string]any{"values": []string{"ads.example"}}).want(http.StatusOK)
	noop, _ := st.ReadGeneration(t.Context())
	if !after.Generation().Same(noop.Generation()) {
		t.Fatal("dedup changed durable generation")
	}
	var policy public.Policy
	if err := st.Get(t.Context(), store.Key{Kind: "Policy", Namespace: "default", Name: "file"}, &policy); err != nil || len(policy.Spec.AllowListRefs) != 0 {
		t.Fatal("file policy changed", err)
	}
	h.do(http.MethodPatch, "/api/v1/allowlist/"+id, "operator", map[string]any{"scope": "groups", "groups": []string{"kids"}, "type": "wildcard", "value": "*.Renamed.Example.", "enabled": false, "comment": "moved"}).want(http.StatusOK).json(fresh(&out))
	if len(out.Items) != 1 || out.Items[0].Value != "*.renamed.example" || out.Items[0].Everyone || out.Items[0].Enabled || !slices.Equal(out.Items[0].Groups, []string{"default/kids"}) {
		t.Fatalf("bad move: %+v", out)
	}
	id = out.Items[0].ID
	if err := st.Get(t.Context(), store.Key{Kind: "BlockList", Namespace: "default", Name: "ui-allow"}, &public.BlockList{}); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("empty old list retained", err)
	}
	h.do(http.MethodPost, "/api/v1/allowlist:batch", "operator", map[string]any{"ids": []string{id, "bad"}, "op": "enable"}).want(http.StatusOK).json(fresh(&out))
	if len(out.Processed.Success) != 1 || len(out.Processed.Errors) != 1 || !out.Items[0].Enabled {
		t.Fatalf("bad batch: %+v", out)
	}
	h.do(http.MethodDelete, "/api/v1/allowlist/"+id, "operator", nil).want(http.StatusNoContent)
	if err := st.Get(t.Context(), store.Key{Kind: "BlockList", Namespace: "default", Name: "ui-allow-kids"}, &public.BlockList{}); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("last entry did not delete list", err)
	}
	if err := st.Get(t.Context(), store.Key{Kind: "Policy", Namespace: "default", Name: "kids"}, &policy); err != nil || len(policy.Spec.AllowListRefs) != 0 {
		t.Fatal("policy did not detach", err)
	}
}

func TestDomainCommandAuthorizationOwnersAndFinalRollback(t *testing.T) {
	denied := false
	calls := 0
	h, st := domainCommandHarness(t, func([]rbac.Attributes) error {
		calls++
		if denied {
			return errForbidden(Missing{Resource: "blocklists", Verb: "write"}, "")
		}
		return nil
	})
	zoneSeed(t, st, store.ManagerAPI, &public.Policy{ObjectMeta: objectMeta("default", "default")}, &public.ClientGroup{ObjectMeta: objectMeta("default", "kids")}, &public.Policy{ObjectMeta: objectMeta("default", "kids"), Spec: public.PolicySpec{ClientGroupRefs: []string{"kids"}}})
	zoneSeed(t, st, store.ManagerAPI, &public.ClientGroup{ObjectMeta: objectMeta("default", "second")}, &public.Policy{ObjectMeta: objectMeta("default", "second"), Spec: public.PolicySpec{ClientGroupRefs: []string{"second"}}})
	zoneSeed(t, st, "file:locked.yaml", &public.BlockList{ObjectMeta: objectMeta("default", "ui-allow-second"), Spec: public.BlockListSpec{Mode: "allow", Sources: []public.ListSource{{Name: "ui", Entries: []public.ListEntry{{Value: "other.example"}}}}}})
	before, _ := st.ReadGeneration(t.Context())
	// A later group target is foreign-owned: no earlier detached list/attachment
	// may leak into SQLite, even when all operation permissions are present.
	h.do(http.MethodPost, "/api/v1/allowlist", "operator", map[string]any{"values": []string{"a.example"}, "groups": []string{"kids", "second"}}).want(http.StatusConflict)
	denied = true
	for _, body := range []map[string]any{{"values": []string{"bad value"}}, {"values": []string{"a.example"}}} {
		h.do(http.MethodPost, "/api/v1/allowlist", "operator", body).want(http.StatusForbidden)
	}
	denied = false
	command := h.srv.opts.DomainCommand
	h.srv.opts.DomainCommand = func(ctx context.Context, target DomainCommandTarget, prepare DomainCommandPrepare) error {
		return command(ctx, target, func(ctx context.Context, tx *sqlite.CommandTx) (*DomainCommandPlan, error) {
			if err := tx.RequireCommitCheck(func(context.Context) error { return errors.New("final admission refused") }); err != nil {
				return nil, err
			}
			return prepare(ctx, tx)
		})
	}
	h.do(http.MethodPost, "/api/v1/domains/allow/exact", "operator", map[string]any{"domains": []string{"a.example", "*.b.example"}}).want(http.StatusInternalServerError)
	after, _ := st.ReadGeneration(t.Context())
	if !before.Generation().Same(after.Generation()) || calls != 4 {
		t.Fatal("refused domain command changed state or skipped authority", calls)
	}
	h.srv.opts.DomainCommand = nil
	h.do(http.MethodPost, "/api/v1/allowlist", "operator", map[string]any{"values": []string{"a.example"}}).want(http.StatusServiceUnavailable)
}

func TestDomainCommandExceptionsAndFailedBatchItemIsolation(t *testing.T) {
	h, st := domainCommandHarness(t, nil)
	zoneSeed(t, st, store.ManagerAPI, &public.Policy{ObjectMeta: objectMeta("default", "default")})
	zoneSeed(t, st, store.ManagerAPI, &public.BlockList{ObjectMeta: objectMeta("default", "feed"), Spec: public.BlockListSpec{Sources: []public.ListSource{{Name: "url", URL: "https://lists.example/list.txt", Format: "domains", Exclude: []string{"ads.example"}}}}})
	id := domainlist.ExceptionID("default", "feed", "url", "ads.example")
	var out DomainWriteResult
	h.do(http.MethodPatch, "/api/v1/allowlist/"+id, "operator", map[string]any{"enabled": false}).want(http.StatusOK).json(&out)
	if len(out.Items) != 1 || out.Items[0].Enabled || len(domainListAt(t, st, "feed").Spec.Sources[0].Exclude) != 0 {
		t.Fatalf("exception enable failed %+v", out)
	}
	h.do(http.MethodPatch, "/api/v1/allowlist/"+id, "operator", map[string]any{"enabled": true}).want(http.StatusOK)
	h.do(http.MethodDelete, "/api/v1/allowlist/"+id, "operator", nil).want(http.StatusNoContent)
	// A switched-back regex exception keeps the selected source's parser
	// identity even though it is absent from the active exception inventory.
	pattern := `^ads[0-9]+\.example$`
	zoneSeed(t, st, store.ManagerAPI, &public.BlockList{ObjectMeta: objectMeta("default", "regex-feed"), Spec: public.BlockListSpec{Sources: []public.ListSource{{Name: "regex", URL: "https://lists.example/regex", Format: "regex", Exclude: []string{pattern}}}}})
	h.do(http.MethodPatch, "/api/v1/allowlist/"+domainlist.ExceptionID("default", "regex-feed", "regex", pattern), "operator", map[string]any{"enabled": false}).want(http.StatusOK).json(fresh(&out))
	if len(out.Items) != 1 || out.Items[0].Type != "regex" || out.Items[0].Enabled {
		t.Fatal("disabled exception lost regex identity", out)
	}
	zoneSeed(t, st, store.ManagerAPI, &public.BlockList{ObjectMeta: objectMeta("default", "rpz-feed"), Spec: public.BlockListSpec{Sources: []public.ListSource{{Name: "rpz", Format: "rpz", RPZ: &public.RPZSource{ZoneName: "policy.example.", Primaries: []string{"primary.example:5300"}, TransferProtocol: "tcp"}}}}})
	beforeRPZ, _ := st.ReadGeneration(t.Context())
	h.do(http.MethodPatch, "/api/v1/allowlist/"+domainlist.ExceptionID("default", "rpz-feed", "rpz", "ads.example"), "operator", map[string]any{"enabled": false}).want(http.StatusUnprocessableEntity)
	afterRPZ, _ := st.ReadGeneration(t.Context())
	if !beforeRPZ.Generation().Same(afterRPZ.Generation()) {
		t.Fatal("unsupported RPZ exception mutated state")
	}
	// Inline discovery uses captured object reads; mutation reads only tx.
	inline := &public.BlockList{ObjectMeta: objectMeta("default", "inline"), Spec: public.BlockListSpec{Sources: []public.ListSource{{Name: "one", Format: "domains", Inline: []string{"a.example", "b.example"}}}}}
	zoneSeed(t, st, store.ManagerAPI, inline)
	current := domainListAt(t, st, "inline")
	// Supply exact current identity through the existing captured-read seam.
	_, reads := newObjectReadFixture(t, current)
	h.srv.opts.ObjectReads = reads
	h.do(http.MethodPost, "/api/v1/allowlist", "operator", map[string]any{"values": []string{"A.Example.", "missing.example"}, "list": "inline"}).want(http.StatusOK).json(fresh(&out))
	if len(out.Items) != 1 || len(out.Processed.Errors) != 1 || !slices.Equal(domainListAt(t, st, "inline").Spec.Sources[0].Exclude, []string{"a.example"}) {
		t.Fatalf("bad exception create %+v", out)
	}
	h.do(http.MethodPost, "/api/v1/denylist", "operator", map[string]any{"values": []string{"ads.example"}}).want(http.StatusOK).json(fresh(&out))
	normal := out.Items[0].ID
	h.do(http.MethodPost, "/api/v1/denylist:batch", "operator", map[string]any{"ids": []string{normal, id}, "op": "disable"}).want(http.StatusOK).json(fresh(&out))
	if len(out.Processed.Errors) != 1 || len(out.Processed.Success) != 1 || domainListAt(t, st, "ui-deny").Spec.Sources[0].Entries[0].IsEnabled() || len(domainListAt(t, st, "feed").Spec.Sources[0].Exclude) != 0 {
		t.Fatal("failed batch item leaked edits", out)
	}
}

func TestDomainCommandScopedCachePostcommit(t *testing.T) {
	h, _ := domainCommandHarness(t, nil)
	responseCache := cache.New(cache.Options{MaxEntries: 64})
	h.srv.opts.ResponseCache = responseCache
	scope := pipeline.CacheScope{ID: [32]byte{7}, Epoch: 9}
	key := cache.MakeScopedKey(scope, "ads.example.", 1, 1, true, true, netip.MustParsePrefix("192.0.2.0/24"))
	responseCache.SetWithMetadata(key, []byte("cached"), cache.Positive, time.Hour, cache.Metadata{Scope: scope})
	h.do(http.MethodPost, "/api/v1/denylist", "operator", map[string]any{"values": []string{"ads.example"}}).want(http.StatusOK)
	if _, ok := responseCache.Get(key); ok {
		t.Fatal("committed domain edit retained a scoped cached answer")
	}
}

func TestDomainCommandURLReceiptPreflightAndIdentity(t *testing.T) {
	h, st := domainCommandHarness(t, nil)
	body := "0.0.0.0 ads.example\n0.0.0.0 tracker.example\n"
	list := &public.BlockList{ObjectMeta: objectMeta("default", "remote"), Spec: public.BlockListSpec{Sources: []public.ListSource{{Name: "feed", URL: "https://lists.example/hosts", Format: "hosts"}}}}
	zoneSeed(t, st, store.ManagerAPI, list)
	current := domainListAt(t, st, "remote")
	current.Status.Sources = []public.SourceStatus{{Name: "feed", Digest: sourceTestDigest(body)}}
	// Status writes have their own API; preflight only needs the captured receipt.
	_, reads := newObjectReadFixture(t, current)
	h.srv.opts.ObjectReads = reads
	entered := false
	h.srv.opts.SourceBodyReads = &sourceBodyFixture{open: func(context.Context, string, string, string) (io.ReadCloser, string, int64, error) {
		if entered {
			t.Fatal("body scan ran under current transaction pins")
		}
		return io.NopCloser(strings.NewReader(body)), sourceTestDigest(body), int64(len(body)), nil
	}}
	command := h.srv.opts.DomainCommand
	h.srv.opts.DomainCommand = func(ctx context.Context, target DomainCommandTarget, prepare DomainCommandPrepare) error {
		entered = true
		defer func() { entered = false }()
		return command(ctx, target, prepare)
	}
	var out DomainWriteResult
	h.do(http.MethodPost, "/api/v1/allowlist", "operator", map[string]any{"list": "remote", "values": []string{"ADS.EXAMPLE."}}).want(http.StatusOK).json(&out)
	if len(out.Items) != 1 || out.Items[0].Kind != "exception" || !slices.Equal(domainListAt(t, st, "remote").Spec.Sources[0].Exclude, []string{"ads.example"}) {
		t.Fatalf("wrong URL exception %+v", out)
	}
	// The same stale receipt must not discover another value and then edit a
	// replacement list: current RV no longer matches the original publication.
	before, _ := st.ReadGeneration(t.Context())
	h.do(http.MethodPost, "/api/v1/allowlist", "operator", map[string]any{"list": "remote", "values": []string{"tracker.example"}}).want(http.StatusServiceUnavailable)
	after, _ := st.ReadGeneration(t.Context())
	if !before.Generation().Same(after.Generation()) {
		t.Fatal("stale body discovery changed current list")
	}
}

func TestDomainCommandQueryAliasAndMissingResult(t *testing.T) {
	calls := 0
	h, st := domainCommandHarness(t, func([]rbac.Attributes) error { calls++; return nil })
	log, _ := querylogFixture(t)
	h.srv.opts.Querylog = log.Reader()
	h.do(http.MethodPost, "/api/v1/queries/2/actions", "operator", map[string]any{"action": "allow"}).want(http.StatusOK)
	if got := domainListAt(t, st, "ui-allow"); len(got.Spec.Sources[0].Entries) != 1 || got.Spec.Sources[0].Entries[0].Value != "ads.example" {
		t.Fatal("query alias did not normalize current input", got)
	}
	h.do(http.MethodPost, "/api/v1/queries/9999/actions", "operator", map[string]any{"action": "deny"}).want(http.StatusNotFound)
	if calls != 2 {
		t.Fatal("missing query bypassed current authorization", calls)
	}
}

func TestDomainCommandLegacyAliasesAndPlanLifetime(t *testing.T) {
	h, st := domainCommandHarness(t, nil)
	zoneSeed(t, st, store.ManagerAPI, &public.Policy{ObjectMeta: objectMeta("default", "default")})
	h.do(http.MethodPost, "/api/v1/domains/deny/exact", "operator", map[string]any{"domains": []string{"Exact.Example.", "||Wild.Example^", "bad value"}}).want(http.StatusOK)
	list := domainListAt(t, st, "ui-deny")
	if len(list.Spec.Sources[0].Entries) != 2 || list.Spec.Sources[0].Entries[0].Value != "exact.example" || list.Spec.Sources[0].Entries[1].Value != "*.wild.example" {
		t.Fatal("legacy exact/wildcard identity changed", list.Spec.Sources)
	}
	h.do(http.MethodPost, "/api/v1/domains/allow/regex", "operator", map[string]any{"domains": []string{`/^ads[0-9]+\./`}, "list": "custom"}).want(http.StatusOK)
	custom := domainListAt(t, st, "custom")
	if custom.Spec.Mode != "allow" || custom.Spec.Sources[0].Entries[0].Origin != public.OriginAPI {
		t.Fatal("custom legacy list lost mode or provenance", custom)
	}
	var policy public.Policy
	if err := st.Get(t.Context(), store.Key{Kind: "Policy", Namespace: "default", Name: "default"}, &policy); err != nil || !slices.Contains(policy.Spec.AllowListRefs, "custom") {
		t.Fatal("custom legacy list did not attach", err)
	}
	if err := st.Command(t.Context(), st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		target := DomainCommandTarget{domainlist.Allow, "create"}
		plan := &DomainCommandPlan{tx: tx, target: target, apply: func(context.Context, *sqlite.CommandTx) error { return nil }}
		if !plan.PreparedFor(tx, target) || plan.PreparedFor(tx, DomainCommandTarget{domainlist.Deny, "create"}) {
			t.Fatal("plan identity not sealed")
		}
		if err := plan.Apply(ctx, tx); err != nil {
			return err
		}
		if plan.PreparedFor(tx, target) || plan.Apply(ctx, tx) == nil || len(plan.Requirements()) != 0 {
			t.Fatal("used plan remained reusable")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
