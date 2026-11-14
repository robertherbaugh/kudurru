package main

import (
	"bytes"
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/tsigkeys"
)

type startupTSIGReader struct {
	identity func(context.Context) (string, error)
	list     func(context.Context) (store.ObjectList, error)
	reads    atomic.Int32
}

func (r *startupTSIGReader) StartupClusterID(ctx context.Context) (string, error) {
	return r.identity(ctx)
}
func (r *startupTSIGReader) List(ctx context.Context, _ store.Kind, _ store.ListOptions) (store.ObjectList, error) {
	r.reads.Add(1)
	return r.list(ctx)
}

func startupTSIGInputs(t *testing.T) (*tsigkeys.Sealer, *dnsserver.StaticKeyring) {
	t.Helper()
	sealer, err := tsigkeys.NewSealer(bytes.Repeat([]byte{7}, 32), nil)
	if err != nil {
		t.Fatal(err)
	}
	ring, err := dnsserver.NewStaticKeyring(dnsserver.TSIGKey{Name: "file.", Algorithm: dns.HmacSHA256, Secret: bytes.Repeat([]byte{8}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	return sealer, ring
}

func awaitStartupTSIG(t *testing.T, source *kubeTSIGStartup) {
	t.Helper()
	select {
	case <-source.done:
	case <-time.After(2 * time.Second):
		t.Fatal("original startup work did not join")
	}
}

func TestKubeTSIGStartupUnknownIsNotFileFallback(t *testing.T) {
	seal, baseline := startupTSIGInputs(t)
	entered := make(chan struct{})
	reader := &startupTSIGReader{identity: func(ctx context.Context) (string, error) {
		close(entered)
		<-ctx.Done()
		return "", ctx.Err()
	}, list: func(context.Context) (store.ObjectList, error) { return store.ObjectList{}, nil }}
	source, err := newKubeTSIGStartup(t.Context(), reader, seal, baseline, 80*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(source.Close)
	<-entered
	unsigned := new(dns.Msg).SetQuestion("example.", dns.TypeA)
	signed := unsigned.Copy()
	signed.SetTsig("file.", dns.HmacSHA256, 30, time.Now().Unix())
	for phase := 0; phase < 2; phase++ {
		if keys, err := source.requestKeys(&pipeline.Request{Ctx: t.Context(), Msg: signed}); keys != nil || !errors.Is(err, errKubeTSIGUnavailable) {
			t.Fatal("unknown source adopted file signer")
		}
		keys, err := source.requestKeys(&pipeline.Request{Ctx: t.Context(), Msg: unsigned})
		if err != nil || keys == nil || len(keys.Names()) != 0 {
			t.Fatal("unrelated unsigned DNS lost availability")
		}
		dz := &v1alpha1.DNSZone{Spec: v1alpha1.DNSZoneSpec{Secondary: &v1alpha1.SecondarySpec{TSIGKeyRef: "file."}}}
		if source.captureAcquisition(dz) != nil {
			t.Fatal("unknown source admitted signed acquisition")
		}
		dz.Spec.Secondary.TSIGKeyRef = ""
		if cut := source.captureAcquisition(dz); cut == nil || !cut.Current() || len(cut.Keys().Names()) != 0 {
			t.Fatal("unsigned acquisition blocked")
		}
		awaitStartupTSIG(t, source)
	}
	if reader.reads.Load() != 0 || source.result.Load().prepared != nil || source.changes() != nil {
		t.Fatal("failed identity became prepared source or busy wake")
	}
}

func TestKubeTSIGStartupCompleteEmptyAndTerminalFailure(t *testing.T) {
	for _, mode := range []string{"empty", "read failure", "truncated", "malformed", "late success", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			seal, baseline := startupTSIGInputs(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if mode == "canceled" {
				cancel()
			}
			reader := &startupTSIGReader{identity: func(context.Context) (string, error) { return "actual-cluster", nil }, list: func(ctx context.Context) (store.ObjectList, error) {
				switch mode {
				case "read failure":
					return store.ObjectList{}, errors.New("source unavailable")
				case "truncated":
					return store.ObjectList{Continue: "more"}, nil
				case "malformed":
					return store.ObjectList{Items: []store.Object{&v1alpha1.DNSZone{}}}, nil
				case "late success":
					<-ctx.Done()
				}
				return store.ObjectList{}, nil
			}}
			source, err := newKubeTSIGStartup(ctx, reader, seal, baseline, 40*time.Millisecond)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(source.Close)
			awaitStartupTSIG(t, source)
			if got := source.keys(); (got != nil) != (mode == "empty") {
				t.Fatal("complete versus unavailable classification lost")
			}
			if mode == "empty" {
				if _, _, ok := source.keys().Lookup("file."); !ok {
					t.Fatal("verified complete empty source lost file keys")
				}
				// Successful work cancellation does not expire its immutable result.
				cut := source.captureAcquisition(&v1alpha1.DNSZone{Spec: v1alpha1.DNSZoneSpec{Secondary: &v1alpha1.SecondarySpec{TSIGKeyRef: "file."}}})
				if cut == nil || !cut.Current() {
					t.Fatal("successful startup expired with preparation")
				}
				published := false
				if err := cut.WithCurrent(t.Context(), func() error { source.Close(); return nil }, func() error { published = true; return nil }); err == nil || published {
					t.Fatal("closed source published acquisition")
				}
			}
			before := reader.reads.Load()
			for range 10 {
				_ = source.keys()
			}
			if reader.reads.Load() != before {
				t.Fatal("request-time recovery read")
			}
		})
	}
}

func TestKubeTSIGStartupCloseJoinsAndCancelsNotifier(t *testing.T) {
	seal, baseline := startupTSIGInputs(t)
	entered := make(chan struct{})
	reader := &startupTSIGReader{identity: func(ctx context.Context) (string, error) { close(entered); <-ctx.Done(); return "", ctx.Err() }}
	source, err := newKubeTSIGStartup(t.Context(), reader, seal, baseline, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(source.Close)
	<-entered
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := source.runNotifier(ctx, func(context.Context) error { t.Error("canceled notifier entered"); return nil }); err != nil {
		t.Fatal(err)
	}
	source.Close()
	awaitStartupTSIG(t, source)
	if source.keys() != nil {
		t.Fatal("closed source retained signer")
	}
}
