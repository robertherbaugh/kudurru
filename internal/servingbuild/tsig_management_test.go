package servingbuild

import (
	"bytes"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/miekg/dns"
	kinds "github.com/robertherbaugh/kudurru/api/tsigkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/tsigkeys"
)

func TestTSIGManagementOriginalLifetimeAndIndependentRingCurrency(t *testing.T) {
	source, err := tsigkeys.FromImage(t.Context(), &replication.Image{Complete: true, Term: replication.Term{ClusterID: "management-capture"}, KindManifest: []replication.Kind{{APIVersion: public.GroupVersion.String(), Kind: kinds.KindTSIGKey, Count: 0}}})
	if err != nil {
		t.Fatal("prepare explicit empty source")
	}
	sealer, err := tsigkeys.NewSealer(bytes.Repeat([]byte{8}, 32), nil)
	if err != nil {
		t.Fatal("prepare original encryption owner")
	}
	baseline, err := dnsserver.NewStaticKeyring(dnsserver.TSIGKey{Name: "file.", Algorithm: dns.HmacSHA256, Secret: bytes.Repeat([]byte{7}, 32)})
	if err != nil {
		t.Fatal("prepare immutable baseline")
	}
	prepared, err := source.Prepare(sealer, baseline)
	if err != nil {
		t.Fatal("prepare complete original")
	}
	var host, provider, ring atomic.Bool
	host.Store(true)
	provider.Store(true)
	ring.Store(true)
	capability, err := NewTSIGManagement(prepared, sealer, host.Load)
	if err != nil {
		t.Fatal("bind original inputs")
	}
	capability = capability.BindLifetime(provider.Load)
	owner := &RuntimeOwner{tsig: capability, current: func() bool { return host.Load() && provider.Load() && ring.Load() }}
	metadata, err := owner.TSIGMetadata()
	if err != nil || len(metadata) != 1 || metadata[0].Source != "file" {
		t.Fatal("original metadata absent")
	}
	metadata[0].KeyName = "mutated."
	fresh, _ := owner.TSIGMetadata()
	if fresh[0].KeyName != "file." {
		t.Fatal("metadata alias escaped immutable source")
	}
	if _, err := owner.PlanTSIGChange(tsigkeys.MutationInput{Operation: tsigkeys.Create, KeyName: "new.", Algorithm: dns.HmacSHA256, Secret: bytes.Repeat([]byte{4}, 32)}); err != nil {
		t.Fatal("bounded original plan refused")
	}
	ring.Store(false)
	if !owner.TSIGOriginalCurrent() || !owner.MatchesTSIGTarget(source.Identity(), sealer.Identity()) {
		t.Fatal("own ring change destroyed original host/provider lifetime")
	}
	if _, err := owner.PlanTSIGChange(tsigkeys.MutationInput{}); !errors.Is(err, serving.ErrDependency) {
		t.Fatal("old ring regained planning authority")
	}
	if _, err := owner.TSIGMetadata(); !errors.Is(err, serving.ErrDependency) {
		t.Fatal("superseded protected metadata read accepted")
	}
	provider.Store(false)
	if owner.TSIGOriginalCurrent() {
		t.Fatal("released/replaced provider retained acknowledgment lifetime")
	}
	provider.Store(true)
	host.Store(false)
	if owner.TSIGOriginalCurrent() {
		t.Fatal("replaced host/encryption retained acknowledgment lifetime")
	}
	if owner.MatchesTSIGTarget("wrong-source", sealer.Identity()) || owner.MatchesTSIGTarget(source.Identity(), "wrong-encryption") {
		t.Fatal("inexact target matched")
	}
	if (*TSIGManagement)(nil).BindLifetime(func() bool { return true }) != nil {
		t.Fatal("nil capability repaired")
	}
	if _, err := NewTSIGManagement(prepared, sealer, func() bool { return false }); err == nil {
		t.Fatal("ended original source captured")
	}
}
