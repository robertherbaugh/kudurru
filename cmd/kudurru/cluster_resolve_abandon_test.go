package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// resolveAbandonOffline is the finite lifetime an offline command gives the store.
func resolveAbandonOffline(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// A member fenced by a newer term whose image it never installed is stranded
// when that term's primary and every member that followed it are lost for
// good (for a managed member that refuses that primary's branch, D369). The
// offline review names the state, and the actual CLI keeps the member's branch
// only with the exact --abandon-term epoch, committing an audited new term.
// The member is a real receiver joined through the actual CLI and TLS.
func TestClusterResolveAbandonsTheLostTermOfAFencedMember(t *testing.T) {
	p, _, issue := joinPrimaryFixture(t)
	inv := issue("receiver")
	wire, err := inv.Encode()
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "invitation")
	if err := os.WriteFile(file, []byte(wire), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "receiver")
	run := func(args ...string) (string, error) {
		t.Helper()
		var out bytes.Buffer
		cmd := newClusterCmd(&cliOpts{})
		cmd.SetOut(&out)
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs(args)
		err := cmd.ExecuteContext(t.Context())
		return out.String(), err
	}
	if _, err := run("join", "--offline", "--data-dir", dir, "--invitation-file", file, "--confirm-name", "receiver", "--confirm-address", "127.0.0.2:8082"); err != nil {
		t.Fatal("actual CLI join failed", err)
	}
	review := func() (clusterResolutionReview, string) {
		t.Helper()
		raw, err := run("resolution", "--offline", "--data-dir", dir)
		if err != nil {
			t.Fatal(err)
		}
		var out clusterResolutionReview
		if err := json.Unmarshal([]byte(raw), &out); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "resolution.json")
		if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		return out, path
	}
	if following, _ := review(); following.Mode != string(replication.Follower) || following.Resolvable || following.Reason != "no_conflict" {
		t.Fatal("a following member is resolvable", following.Mode, following.Reason)
	}
	// The member observes a newer term of its primary and is fenced by it; it
	// never installs that term's image.
	s, node, err := openOfflineMember(resolveAbandonOffline(t), dir)
	if err != nil {
		t.Fatal(err)
	}
	installed := s.Authority().Installed
	lost := installed.Term
	lost.Epoch++
	lost.PromotionID, lost.PrimaryID, lost.PrimaryName = uuid.NewString(), p.opts.Store.Authority().MemberID, "lost-primary"
	if _, err := s.ObserveAuthority(resolveAbandonOffline(t), lost); err != nil || s.Authority().Mode != replication.Fenced {
		t.Fatal("fixture member was not fenced by the newer term", err)
	}
	if err := s.Close(); err != nil || node != "receiver" {
		t.Fatal(err, node)
	}
	fenced, path := review()
	if fenced.Mode != string(replication.Fenced) || !fenced.Resolvable || fenced.Reason != resolutionFencedBehindTerm || !fenced.Term.Same(lost) ||
		!fenced.Installed.Same(installed) || fenced.Conflict != nil || fenced.Divergence != nil {
		t.Fatal("offline review does not name a fenced member behind its term", fenced.Mode, fenced.Reason)
	}
	durable := func() replication.Authority {
		t.Helper()
		s, _, err := openOfflineMember(resolveAbandonOffline(t), dir)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = s.Close() }()
		return s.Authority()
	}
	before := durable()
	base := []string{"resolve", "--offline", "--data-dir", dir, "--review", path, "--confirm-node", "receiver", "--request-id", "abandon-lost", "--reason", "primary and followers lost"}
	for _, extra := range [][]string{nil, {"--abandon-term", strconv.FormatUint(lost.Epoch+1, 10)}, {"--abandon-term", strconv.FormatUint(installed.Term.Epoch, 10)}} {
		out, err := run(append(append([]string{}, base...), extra...)...)
		if err == nil || strings.Contains(out, "committed") || !reflect.DeepEqual(before, durable()) {
			t.Fatal("resolution without the exact abandoned epoch was not refused unchanged", extra, err)
		}
		if extra == nil && !strings.Contains(err.Error(), "--abandon-term") {
			t.Fatal("refusal does not name the remedy", err)
		}
	}
	raw, err := run(append(append([]string{}, base...), "--abandon-term", strconv.FormatUint(lost.Epoch, 10))...)
	if err != nil {
		t.Fatal("abandonment of the lost term failed", err)
	}
	var receipt clusterResolutionReceipt
	if err := json.Unmarshal([]byte(raw), &receipt); err != nil || !receipt.Committed || receipt.Term.Epoch != lost.Epoch+1 || receipt.Term.PrimaryName != "receiver" ||
		receipt.Term.PrimaryID != before.MemberID || !receipt.Kept.Same(installed) {
		t.Fatal("abandonment did not allocate a new term over the member's own branch", err, raw)
	}
	after := durable()
	if after.Mode != replication.Primary || !after.Term.Same(receipt.Term) || after.Conflict != nil || after.Divergence != nil {
		t.Fatal("durable authority is not the new primary term", after.Mode)
	}
	s, _, err = openOfflineMember(resolveAbandonOffline(t), dir)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := s.ReadAudit(t.Context(), sqlite.AuditQuery{})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if e.Action == "cluster.resolve" && e.RequestID == "abandon-lost" {
			n++
			if e.Actor != "host:offline-cli" || e.Reason != "primary and followers lost" || e.Authz != "host-local data directory and member identity; abandons unreachable term "+strconv.FormatUint(lost.Epoch, 10) {
				t.Fatal("abandonment audit attribution", e.Actor, e.Authz)
			}
		}
	}
	if err := s.Close(); err != nil || n != 1 {
		t.Fatal("abandonment audit count", n, err)
	}
	// The saved review is stale now, and a replay is refused.
	if _, err := run(append(append([]string{}, base...), "--abandon-term", strconv.FormatUint(lost.Epoch, 10))...); err == nil || !reflect.DeepEqual(after, durable()) {
		t.Fatal("a stale abandonment review was replayed", err)
	}
	if primary, _ := review(); primary.Mode != string(replication.Primary) || primary.Resolvable {
		t.Fatal("the new primary reads as resolvable", primary.Mode, primary.Reason)
	}
}
