package cloudflare

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/integration/host"
)

func TestSDKPreviewApplyPreservesUnchangedIDsAndReceipts(t *testing.T) {
	f := newFixture(t)
	stable := f.add("www.example.com.", "A", "192.0.2.1", 300, marker(f.h.owner))
	removed := f.add("www.example.com.", "A", "192.0.2.2", 300, marker(f.h.owner))
	e := f.adjusted(endpoint("192.0.2.1", "192.0.2.3"))
	op := f.intent(e)
	r, err := f.p.Apply(f.ctx, op)
	if err != nil || !r.Complete || r.ContentHash != op.Spec.AfterHash || len(r.ProviderIDs) != 2 {
		t.Fatal(r, err)
	}
	op.Spec.Receipt = &r
	if err = op.Validate(); err != nil {
		t.Fatal("receipt exceeds durable contract", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rows[stable] == nil || f.rows[removed] != nil || len(f.rows) != 2 || len(f.batches) != 1 || len(f.batches[0]["deletes"]) != 1 || len(f.batches[0]["posts"]) != 1 {
		t.Fatal("incorrect batch or IDs", f.batches)
	}
}
func TestSDKAdoptionPatchAndMetadataPut(t *testing.T) {
	for _, external := range []bool{false, true} {
		t.Run(fmt.Sprint(external), func(t *testing.T) {
			z := public.CloudflareZone{Zone: "example.com", ZoneID: zoneID, AdoptUnowned: !external}
			if external {
				z.AdoptFrom = &public.PublicationAdoption{ExternalDNSOwner: "old-owner"}
			}
			f := newFixture(t, z)
			id := f.add("www.example.com.", "A", "192.0.2.1", 300, "")
			if external {
				f.add("www.example.com.", "TXT", `"heritage=external-dns,external-dns/owner=old-owner,external-dns/resource=service/default/web"`, 300, "")
			}
			e := f.adjusted(endpoint("192.0.2.1"))
			op := f.intent(e)
			if op.Spec.Action != "adopt" {
				t.Fatal(op.Spec.Action)
			}
			r, err := f.p.Apply(f.ctx, op)
			if err != nil || !r.Complete || !slices.Equal(r.ProviderIDs, []string{id}) {
				t.Fatal(r, err)
			}
			e.RecordTTL = 600
			e.ProviderSpecific = append(e.ProviderSpecific, public.ProviderSpecific{Name: "cloudflare/comment", Value: "human  comment"})
			op = f.intent(e)
			r, err = f.p.Apply(f.ctx, op)
			if err != nil || !r.Complete || !slices.Equal(r.ProviderIDs, []string{id}) {
				t.Fatal(r, err)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if len(f.batches) != 2 || len(f.batches[0]["patches"]) != 1 || len(f.batches[1]["puts"]) != 1 {
				t.Fatal(f.batches)
			}
		})
	}
}
func TestSDKSplit256EffectsAndRestartAfterFirstBatch(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(fmt.Sprint(restart), func(t *testing.T) {
			f := newFixture(t)
			next := []string{}
			for i := 1; i <= 128; i++ {
				a := fmt.Sprintf("192.0.2.%d", i)
				b := fmt.Sprintf("198.51.100.%d", i)
				next = append(next, b)
				f.add("www.example.com.", "A", a, 300, marker(f.h.owner))
			}
			op := f.intent(f.adjusted(endpoint(next...)))
			if restart {
				f.h.mu.Lock()
				f.h.failAfterWrites = 1
				f.h.mu.Unlock()
			}
			r, err := f.p.Apply(f.ctx, op)
			if restart {
				if !errors.Is(err, host.ErrFenced) || r.Complete || len(r.Targets) != 72 {
					t.Fatal("partial facts lost", r, err)
				}
				op.Spec.Receipt = &r
				if err = op.Validate(); err != nil {
					t.Fatal(err)
				}
				f.h.mu.Lock()
				f.h.failAfterWrites = 0
				f.h.mu.Unlock()
				r, err = f.p.Recover(f.ctx, op, host.RemoteSnapshot{Complete: true})
			}
			if err != nil || !r.Complete || len(r.Targets) != 128 || len(r.ProviderIDs) != 128 {
				t.Fatal("split failed", len(r.Targets), r.Complete, err)
			}
			op.Spec.Receipt = &r
			if err = op.Validate(); err != nil {
				t.Fatal("invalid split receipt", err)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if len(f.batches) != 2 || len(f.batches[0]["deletes"]) != 128 || len(f.batches[0]["posts"]) != 72 || len(f.batches[1]["posts"]) != 56 || len(f.rows) != 128 {
				t.Fatal("bad split", len(f.batches))
			}
		})
	}
}
func TestSDKAmbiguousCommitNeverBlindlyRecreates(t *testing.T) {
	for _, mode := range []string{"500", "eof", "malformed", "wrong-response", "wrong-state"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			op := f.intent(f.adjusted(endpoint("192.0.2.1")))
			f.mu.Lock()
			f.hook = func(w http.ResponseWriter, r *http.Request) bool {
				if !strings.HasSuffix(r.URL.Path, "/batch") {
					return false
				}
				var batch map[string][]map[string]any
				_ = json.NewDecoder(r.Body).Decode(&batch)
				f.batches = append(f.batches, batch)
				result := f.applyBatch(batch)
				switch mode {
				case "500":
					w.WriteHeader(500)
					_, _ = w.Write([]byte(testToken))
				case "eof":
					conn, _, err := w.(http.Hijacker).Hijack()
					if err == nil {
						_ = conn.Close()
					}
				case "malformed":
					_, _ = w.Write([]byte(`{"success":true,"result":`))
				case "wrong-response":
					result["posts"] = []any{map[string]any{"id": strings.Repeat("f", 32), "name": "other.example.com", "type": "A", "ttl": 300, "proxied": false, "content": "192.0.2.99"}}
					success(w, result)
				case "wrong-state":
					for _, row := range f.rows {
						row["content"] = "192.0.2.99"
					}
					success(w, result)
				}
				return true
			}
			f.mu.Unlock()
			r, err := f.p.Apply(f.ctx, op)
			if mode == "wrong-state" {
				if err == nil || r.Complete {
					t.Fatal("desired echo accepted", r, err)
				}
				return
			}
			if err != nil || !r.Complete {
				t.Fatal("committed ambiguity not observed", r, err)
			}
			again, err := f.p.Recover(f.ctx, op, host.RemoteSnapshot{Complete: true})
			if err != nil || !again.Complete {
				t.Fatal(again, err)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if len(f.batches) != 1 || len(f.rows) != 1 {
				t.Fatal("create replayed", f.batches)
			}
		})
	}
}
func TestSDKPartialBatchRecoveryRequiresExactOldOrNewMetadata(t *testing.T) {
	f := newFixture(t)
	f.add("www.example.com.", "A", "192.0.2.1", 300, marker(f.h.owner))
	f.add("www.example.com.", "A", "192.0.2.2", 300, marker(f.h.owner))
	e := f.adjusted(endpoint("192.0.2.1", "192.0.2.2"))
	e.RecordTTL = 600
	op := f.intent(e)
	f.mu.Lock()
	f.hook = func(w http.ResponseWriter, r *http.Request) bool {
		if !strings.HasSuffix(r.URL.Path, "/batch") {
			return false
		}
		var batch map[string][]map[string]any
		_ = json.NewDecoder(r.Body).Decode(&batch)
		f.batches = append(f.batches, batch)
		batch["puts"] = batch["puts"][:1]
		f.applyBatch(batch)
		w.WriteHeader(500)
		return true
	}
	f.mu.Unlock()
	r, err := f.p.Apply(f.ctx, op)
	if err == nil || r.Complete || len(r.Targets) != 2 || !r.Targets[0].Applied || r.Targets[1].Applied {
		t.Fatal(r, err)
	}
	op.Spec.Receipt = &r
	f.mu.Lock()
	f.hook = nil
	f.mu.Unlock()
	r, err = f.p.Recover(f.ctx, op, host.RemoteSnapshot{Complete: true})
	if err != nil || !r.Complete {
		t.Fatal("mixed recovery", r, err)
	}
}
func TestSDKSuccessfulFactsSurviveFailedReadAndFences(t *testing.T) {
	for _, reject := range []string{"read", "uid", "source"} {
		t.Run(reject, func(t *testing.T) {
			f := newFixture(t)
			op := f.intent(f.adjusted(endpoint("192.0.2.1")))
			f.mu.Lock()
			f.hook = func(w http.ResponseWriter, r *http.Request) bool {
				if strings.HasSuffix(r.URL.Path, "/dns_records") && len(f.batches) > 0 {
					w.WriteHeader(503)
					return true
				}
				return false
			}
			f.mu.Unlock()
			r, err := f.p.Apply(f.ctx, op)
			if err == nil || r.Complete || len(r.Targets) != 1 || !r.Targets[0].Applied {
				t.Fatal("lost facts", r, err)
			}
			op.Spec.Receipt = &r
			f.mu.Lock()
			f.hook = nil
			f.mu.Unlock()
			switch reject {
			case "uid":
				op.Spec.Record.Owner.UID = "recreated"
			case "source":
				f.h.mu.Lock()
				f.h.failAfterWrites = 1
				f.h.mu.Unlock()
			}
			r, err = f.p.Recover(f.ctx, op, host.RemoteSnapshot{Complete: true})
			if reject == "read" {
				if err != nil || !r.Complete {
					t.Fatal(r, err)
				}
			} else if err == nil {
				t.Fatal("stale recovery accepted")
			}
		})
	}
}
func TestSDKRejectChangedBaselineBeforeMutating(t *testing.T) {
	for _, change := range []string{"content", "version", "foreign", "cname", "unexpected-recovery"} {
		t.Run(change, func(t *testing.T) {
			f := newFixture(t)
			id := f.add("www.example.com.", "A", "192.0.2.1", 300, marker(f.h.owner))
			op := f.intent(f.adjusted(endpoint("192.0.2.2")))
			f.mu.Lock()
			switch change {
			case "content", "unexpected-recovery":
				f.rows[id]["content"] = "192.0.2.99"
			case "version":
				f.rows[id]["modified_on"] = time.Now().Format(time.RFC3339)
			case "foreign":
				f.rows[id]["comment"] = "kudurru:foreign"
			case "cname":
				f.rows[id]["type"] = "CNAME"
				f.rows[id]["content"] = "target.example.com"
			}
			f.mu.Unlock()
			var err error
			if change == "unexpected-recovery" {
				_, err = f.p.Recover(f.ctx, op, host.RemoteSnapshot{Complete: true})
			} else {
				_, err = f.p.Apply(f.ctx, op)
			}
			if err == nil {
				t.Fatal("drift accepted")
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if len(f.batches) != 0 {
				t.Fatal("mutation before validation")
			}
		})
	}
}

func TestSDKAmbiguousFailureWithOldReadCannotAppearUntouched(t *testing.T) {
	f := newFixture(t)
	op := f.intent(f.adjusted(endpoint("192.0.2.1")))
	f.mu.Lock()
	f.hook = func(w http.ResponseWriter, r *http.Request) bool {
		if !strings.HasSuffix(r.URL.Path, "/batch") {
			return false
		}
		var batch map[string][]map[string]any
		_ = json.NewDecoder(r.Body).Decode(&batch)
		f.batches = append(f.batches, batch)
		w.WriteHeader(500)
		return true
	}
	f.mu.Unlock()
	r, err := f.p.Apply(f.ctx, op)
	if err == nil || r.Complete || len(r.Targets) != 1 || r.Targets[0].Applied || r.Targets[0].ErrorClass != "ambiguous" {
		t.Fatal("ambiguous attempt became untouched", r, err)
	}
	op.Spec.Receipt = &r
	if err = op.Validate(); err != nil {
		t.Fatal("ambiguous receipt invalid", err)
	}
}
func TestSDKCadenceCancellationAndDeleteReceipt(t *testing.T) {
	f := newFixture(t)
	e := f.adjusted(endpoint("192.0.2.1"))
	op := f.intent(e)
	f.p.lastBatch = time.Now()
	f.p.cadence = time.Hour
	ctx, cancel := context.WithTimeout(f.ctx, 10*time.Millisecond)
	defer cancel()
	if _, err := f.p.Apply(ctx, op); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("cadence ignored original deadline", err)
	}
	f.p.cadence = 0
	r, err := f.p.Apply(f.ctx, op)
	if err != nil || !r.Complete {
		t.Fatal(r, err)
	}
	deletion := f.intent()
	r, err = f.p.Apply(f.ctx, deletion)
	if err != nil || !r.Complete || r.ContentHash != "" || len(r.ProviderIDs) != 0 || len(r.Targets) != 1 || !r.Targets[0].Applied {
		t.Fatal("delete not observed", r, err)
	}
	deletion.Spec.Receipt = &r
	if err = deletion.Validate(); err != nil {
		t.Fatal(err)
	}
}
