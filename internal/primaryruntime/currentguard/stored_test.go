package currentguard

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/primaryruntime/sqliteadapter"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/tsigkeys"
)

func TestActualStoredSourceLagDeniesUpdateReplayNoopAndBackground(t *testing.T) {
	for _, mode := range []string{"write", "dedup", "noop", "background"} {
		t.Run(mode, func(t *testing.T) {
			f := newSQLiteFixture(t)
			l := f.active("primary", "example.")
			cut, err := tsigkeys.Read(t.Context(), f.s, f.s.Info().ClusterID)
			if err != nil {
				t.Fatal(err)
			}
			// This actual root pin still considers its immutable provider current;
			// no coordinator replacement has occurred when the durable row changes.
			f.h.current.CheckTx = cut.CheckTx
			request := f.request(l, "stored-source-lag", "dynamic.example. 60 IN A 192.0.2.1")
			if mode == "dedup" || mode == "noop" {
				if out := f.r.Apply(f.ctx(), request); out.Rcode != dns.RcodeSuccess {
					t.Fatal(out)
				}
				if mode == "noop" {
					request = f.request(l, "stored-source-noop", "dynamic.example. 60 IN A 192.0.2.1")
				}
			}
			seal, err := tsigkeys.NewSealer(bytes.Repeat([]byte{0x41}, 32), nil)
			if err != nil {
				t.Fatal(err)
			}
			m, err := tsigkeys.NewMaterial(tsigkeys.Owner{ClusterID: f.s.Info().ClusterID, UID: "actual-store-owner", KeyName: "other.example.", Algorithm: dns.HmacSHA256}, bytes.Repeat([]byte{0x42}, 32))
			if err != nil {
				t.Fatal(err)
			}
			row, err := seal.Seal(m)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.s.Apply(t.Context(), row, store.ApplyOptions{Manager: store.ManagerSystem, CreateOnly: true}); err != nil {
				t.Fatal(err)
			}
			before := f.image()
			if mode == "background" {
				var source public.DNSZone
				if err := f.s.Get(f.ctx(), store.Key{Kind: "DNSZone", Name: "primary"}, &source); err != nil {
					t.Fatal(err)
				}
				err = f.g.Background().WithCurrent(f.ctx(), f.fence, func(check sqliteadapter.BackgroundPolicy) error {
					return f.s.Command(f.ctx(), f.fence.Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
						// Ignore a failed check after staging work: its exact source
						// validation still poisons final commit admission.
						_ = tx.PutSection(ctx, "dedup", replication.Entry{Key: "stored-source-ignored", Data: json.RawMessage(`{}`)})
						_ = check(ctx, tx, sqliteadapter.BackgroundCheck{Lifecycle: l, Source: &source, Reason: "lifecycle"})
						return nil
					})
				})
				if err == nil {
					t.Fatal("lagging background owner admitted")
				}
			} else {
				out := f.r.Apply(f.ctx(), request)
				if out.Rcode == dns.RcodeSuccess || (mode != "dedup" && out.Committed) {
					t.Fatal("lagging keys authorized original command", out)
				}
			}
			if !bytes.Equal(before, f.image()) {
				t.Fatal("failed exact source check changed durable state")
			}
		})
	}
}
