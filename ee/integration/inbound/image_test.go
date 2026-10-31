package inbound

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/integration/host"
	"github.com/robertherbaugh/kudurru/internal/inventory"
	"github.com/robertherbaugh/kudurru/internal/inventory/persistence"
	"github.com/robertherbaugh/kudurru/internal/runtimeowners"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

var imageValidator = sync.OnceValue(func() *validator.Validator {
	v, err := validator.New()
	if err != nil {
		panic(err)
	}
	return v
})

func imageOptions(t *testing.T) ImageOptions {
	t.Helper()
	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	opts := ImageOptions{Scheme: scheme, Validation: replication.Validation{SchemaVersion: store.SchemaVersion, ValidateEntry: runtimeowners.ValidateEntry, ValidateImage: runtimeowners.ValidateImage}}
	// Enumerate LOCAL registered store objects, never the tested image manifest.
	known := scheme.KnownTypes(public.GroupVersion)
	for name, typ := range known {
		if _, ok := known[name+"List"]; !ok {
			continue
		}
		object := reflect.New(typ).Interface()
		if _, ok := object.(store.Object); !ok {
			continue
		}
		if _, ok := object.(metav1.ListInterface); ok {
			continue
		}
		opts.Validation.Kinds = append(opts.Validation.Kinds, replication.Kind{APIVersion: public.GroupVersion.String(), Kind: name})
	}
	slices.SortFunc(opts.Validation.Kinds, func(a, b replication.Kind) int { return strings.Compare(a.Kind, b.Kind) })
	return opts
}

func openImageStore(t *testing.T, path string, mode replication.Mode, complete bool) (*sqlite.Store, ImageOptions) {
	t.Helper()
	opts := imageOptions(t)
	validate := runtimeowners.ValidateImage
	if complete {
		validate = func(image *replication.Image) error {
			_, err := DecodeImage(image, opts)
			return err
		}
	}
	var private []store.Kind
	for _, kind := range runtimekinds.Private() {
		private = append(private, store.Kind(kind))
	}
	s, err := sqlite.Open(path, sqlite.Options{InitialMode: mode, Scheme: opts.Scheme, InternalKinds: private, Validator: imageValidator(), Now: func() time.Time { return time.Unix(100000, 0).UTC().Add(72 * time.Hour) }, ValidateReplicaEntry: runtimeowners.ValidateEntry, ValidateReplicaImage: validate})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, opts
}

func initializeImageStore(t *testing.T, s *sqlite.Store) {
	t.Helper()
	if err := s.Command(t.Context(), s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		return runtimeowners.Initialize(ctx, tx, time.Unix(100000, 0).UTC())
	}); err != nil {
		t.Fatal(err)
	}
}

func readSourceGeneration(t *testing.T, s *sqlite.Store) *replication.Image {
	t.Helper()
	image, err := s.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return image
}

func commitImageSource(t *testing.T, s *sqlite.Store, withDNS bool, uid string) SourceImage {
	t.Helper()
	doc, _, at := sourceDocument(t)
	doc.Owner.ClusterID = s.Authority().Term.ClusterID
	doc.Owner.UID = uid
	for i := range doc.Snapshot.Clients {
		doc.Snapshot.Clients[i].Source = host.SourceID(doc.Owner)
	}
	if withDNS {
		doc.Snapshot.Desired.SourceID = host.SourceID(doc.Owner)
		doc.Allocations[0].Spec.Owner = doc.Owner
		doc.Allocations[0].Spec.Scope.ClusterID = doc.Owner.ClusterID
	} else {
		doc.Snapshot.Desired = nil
		doc.Targets, doc.Objects, doc.Allocations, doc.Groups = nil, nil, nil, nil
	}
	var out SourceImage
	err := s.Command(t.Context(), s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		current, err := persistence.Load(ctx, tx, tx.Authority().ClusterID, at.Add(72*time.Hour))
		if err != nil {
			return err
		}
		admittedAt := at
		if current.UpdatedAt.After(admittedAt) {
			admittedAt = current.UpdatedAt
		}
		if withDNS {
			allocation := &kinds.IntegrationNameAllocation{ObjectMeta: metav1.ObjectMeta{Name: kinds.NameAllocationName(doc.Allocations[0].Spec.Scope, doc.Allocations[0].Spec.Label)}, Spec: doc.Allocations[0].Spec}
			saved, err := tx.Apply(ctx, allocation, store.ApplyOptions{Manager: store.ManagerSystem, CreateOnly: true})
			if err != nil {
				return err
			}
			doc.Allocations[0].Object.Name = saved.GetName()
			doc.Allocations[0].Object.UID = string(saved.GetUID())
			doc.Allocations[0].Object.ResourceVersion = saved.GetResourceVersion()
			doc.Allocations[0].Object.IntentDigest, err = IntentDigest(doc.Allocations[0].Spec)
			if err != nil {
				return err
			}
		}
		encoded, err := EncodeSource(doc, 1, kinds.Digest([]byte("config")), kinds.Digest([]byte("credentials")), admittedAt)
		if err != nil {
			return err
		}
		for _, chunk := range encoded.Chunks {
			saved, err := tx.Apply(ctx, chunk, store.ApplyOptions{Manager: store.ManagerSystem, CreateOnly: true})
			if err != nil {
				return err
			}
			out.Chunks = append(out.Chunks, saved.(*kinds.IntegrationSourceChunk))
		}
		saved, err := tx.Apply(ctx, encoded.Head, store.ApplyOptions{Manager: store.ManagerSystem, CreateOnly: true})
		if err != nil {
			return err
		}
		out.Head = saved.(*kinds.IntegrationSourceHead)
		next, err := inventory.PrepareReplacement(current.State, host.SourceID(doc.Owner), 1, doc.Snapshot.Clients, admittedAt)
		if err != nil {
			return err
		}
		_, err = persistence.Stage(ctx, tx, current.Version, next, admittedAt)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func cloneSourceGeneration(t *testing.T, i *replication.Image) *replication.Image {
	t.Helper()
	raw, err := json.Marshal(i)
	if err != nil {
		t.Fatal(err)
	}
	var out replication.Image
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return &out
}

func replaceImageCarrier(t *testing.T, image *replication.Image, kind string, change func(map[string]any)) {
	t.Helper()
	for i, raw := range image.Payload.Objects {
		var value map[string]any
		if err := json.Unmarshal(raw, &value); err != nil {
			t.Fatal(err)
		}
		if value["kind"] != kind {
			continue
		}
		change(value)
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		image.Payload.Objects[i] = encoded
		return
	}
	t.Fatal("missing carrier", kind)
}

func TestDecodeImageCompleteEmptyAndExplicitLocalRegistry(t *testing.T) {
	s, opts := openImageStore(t, filepath.Join(t.TempDir(), sqlite.FileName), replication.Standalone, true)
	initializeImageStore(t, s)
	image := readSourceGeneration(t, s)
	out, err := DecodeImage(image, opts)
	if err != nil || len(out.Sources) != 0 || len(out.Allocations) != 0 || !out.Base.Same(image.Generation()) {
		t.Fatal("empty complete owners", err)
	}
	for _, kind := range kinds.SourceKinds {
		if !slices.ContainsFunc(image.KindManifest, func(k replication.Kind) bool { return k.Kind == kind && k.Count == 0 }) {
			t.Fatal("missing explicit empty kind", kind)
		}
		bad := cloneSourceGeneration(t, image)
		bad.KindManifest = slices.DeleteFunc(bad.KindManifest, func(k replication.Kind) bool { return k.Kind == kind })
		if err := replication.Seal(bad); err != nil {
			t.Fatal(err)
		}
		if _, err := DecodeImage(bad, opts); err == nil {
			t.Fatal("accepted missing zero manifest", kind)
		}
		local := opts
		local.Validation.Kinds = slices.DeleteFunc(slices.Clone(opts.Validation.Kinds), func(k replication.Kind) bool { return k.Kind == kind })
		if _, err := DecodeImage(image, local); err == nil {
			t.Fatal("source inferred from candidate", kind)
		}
	}
	wrong := opts
	wrong.Scheme = runtime.NewScheme()
	if _, err := DecodeImage(image, wrong); err == nil {
		t.Fatal("unregistered source scheme")
	}
	bad := cloneSourceGeneration(t, image)
	bad.Payload.Objects = nil
	if _, err := DecodeImage(bad, opts); err == nil {
		t.Fatal("missing whole section accepted")
	}
}

func TestDecodeImageOriginalAllocationsRetainedHeadsAndIsolation(t *testing.T) {
	s, opts := openImageStore(t, filepath.Join(t.TempDir(), sqlite.FileName), replication.Standalone, true)
	initializeImageStore(t, s)
	source := commitImageSource(t, s, true, "original")
	// Retain exactly the original payload/chunks after live authority is removed.
	err := s.Command(t.Context(), s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		retained := source.Head.DeepCopy()
		retained.Spec.State, retained.Spec.WithdrawnAt = "retained", retained.Spec.AcceptedAt.Add(time.Hour)
		if _, err := tx.Apply(ctx, retained, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
			return err
		}
		current, err := persistence.Load(ctx, tx, tx.Authority().ClusterID, retained.Spec.WithdrawnAt)
		if err != nil {
			return err
		}
		next, err := inventory.PrepareReplacement(current.State, host.SourceID(retained.Spec.Owner), 2, nil, retained.Spec.WithdrawnAt)
		if err != nil {
			return err
		}
		_, err = persistence.Stage(ctx, tx, current.Version, next, retained.Spec.WithdrawnAt)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	// A recreated Integration UID has independent original source authority.
	commitImageSource(t, s, false, "recreated")
	image := readSourceGeneration(t, s)
	want, _ := json.Marshal(image)
	out, err := DecodeImage(image, opts)
	if err != nil || len(out.Sources) != 2 || len(out.Allocations) != 1 {
		t.Fatal("retained complete image", err)
	}
	for _, v := range out.Sources {
		if v.Head.Spec.Owner.UID != "original" {
			continue
		}
		if v.Head.Spec.State != "retained" || v.Head.Spec.Sequence != 1 {
			t.Fatal("retirement changed payload sequence")
		}
		doc, err := DecodeSource(v)
		if err != nil || !doc.Snapshot.Clients[0].ExpiresAt.Equal(v.Head.Spec.AcceptedAt.Add(time.Hour)) {
			t.Fatal("historical expiry renewed", err)
		}
	}
	out.Sources[0].Chunks[0].Spec.Data[0] = '!'
	out.Allocations[0].Spec.Label = "changed"
	after, _ := json.Marshal(image)
	if !bytes.Equal(want, after) {
		t.Fatal("output aliases input")
	}
	if _, err := DecodeImage(image, opts); err != nil {
		t.Fatal("immutable original damaged", err)
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			if _, err := DecodeImage(image, opts); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
}

func TestDecodeImageRejectsBrokenCrossCarrierEvidence(t *testing.T) {
	s, opts := openImageStore(t, filepath.Join(t.TempDir(), sqlite.FileName), replication.Standalone, true)
	initializeImageStore(t, s)
	commitImageSource(t, s, true, "original")
	good := readSourceGeneration(t, s)
	cases := map[string]func(*replication.Image){
		"missing chunk": func(i *replication.Image) {
			i.Payload.Objects = slices.DeleteFunc(i.Payload.Objects, func(raw json.RawMessage) bool { return bytes.Contains(raw, []byte(`"kind":"IntegrationSourceChunk"`)) })
		},
		"missing allocation": func(i *replication.Image) {
			i.Payload.Objects = slices.DeleteFunc(i.Payload.Objects, func(raw json.RawMessage) bool {
				return bytes.Contains(raw, []byte(`"kind":"IntegrationNameAllocation"`))
			})
		},
		"allocation UID": func(i *replication.Image) {
			replaceImageCarrier(t, i, kinds.KindIntegrationNameAllocation, func(v map[string]any) { v["metadata"].(map[string]any)["uid"] = "recreated" })
		},
		"allocation RV": func(i *replication.Image) {
			replaceImageCarrier(t, i, kinds.KindIntegrationNameAllocation, func(v map[string]any) {
				v["metadata"].(map[string]any)["resourceVersion"] = strconv.FormatUint(i.ObjectResourceVersion, 10)
			})
		},
		"allocation source": func(i *replication.Image) {
			replaceImageCarrier(t, i, kinds.KindIntegrationNameAllocation, func(v map[string]any) { v["spec"].(map[string]any)["owner"].(map[string]any)["uid"] = "foreign" })
		},
		"root": func(i *replication.Image) {
			replaceImageCarrier(t, i, kinds.KindIntegrationSourceHead, func(v map[string]any) { v["spec"].(map[string]any)["rootDigest"] = kinds.Digest([]byte("changed")) })
		},
		"wrong cluster": func(i *replication.Image) { i.Term.ClusterID = "other-cluster" },
		"absent carrier UID": func(i *replication.Image) {
			replaceImageCarrier(t, i, kinds.KindIntegrationSourceHead, func(v map[string]any) { delete(v["metadata"].(map[string]any), "uid") })
		},
		"noncanonical RV": func(i *replication.Image) {
			replaceImageCarrier(t, i, kinds.KindIntegrationSourceHead, func(v map[string]any) { v["metadata"].(map[string]any)["resourceVersion"] = "01" })
		},
		"future head": func(i *replication.Image) {
			replaceImageCarrier(t, i, kinds.KindIntegrationSourceHead, func(v map[string]any) {
				v["spec"].(map[string]any)["acceptedAt"] = i.CreatedAt.Add(time.Hour).Format(time.RFC3339Nano)
			})
		},
		"nonzero namespace": func(i *replication.Image) {
			replaceImageCarrier(t, i, kinds.KindIntegrationSourceHead, func(v map[string]any) { v["metadata"].(map[string]any)["namespace"] = "default" })
		},
		"wrong api version": func(i *replication.Image) {
			replaceImageCarrier(t, i, kinds.KindIntegrationSourceHead, func(v map[string]any) { v["apiVersion"] = "kudurru.io/v99" })
		},
		"duplicate": func(i *replication.Image) {
			i.Payload.Objects = append(i.Payload.Objects, slices.Clone(i.Payload.Objects[0]))
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			bad := cloneSourceGeneration(t, good)
			change(bad)
			_ = replication.Seal(bad)
			if _, err := DecodeImage(bad, opts); err == nil {
				t.Fatal("accepted broken evidence")
			}
		})
	}
}

func TestDecodeImageRejectsOriginalCarrierFraming(t *testing.T) {
	s, opts := openImageStore(t, filepath.Join(t.TempDir(), sqlite.FileName), replication.Standalone, true)
	initializeImageStore(t, s)
	commitImageSource(t, s, false, "original")
	good := readSourceGeneration(t, s)
	for _, mode := range []string{"duplicate", "case", "unknown", "missing sequence", "null sequence", "invalid Unicode", "missing creation", "null metadata"} {
		t.Run(mode, func(t *testing.T) {
			bad := cloneSourceGeneration(t, good)
			for i, raw := range bad.Payload.Objects {
				if !bytes.Contains(raw, []byte(`"kind":"IntegrationSourceHead"`)) {
					continue
				}
				switch mode {
				case "duplicate":
					raw = bytes.Replace(raw, []byte(`"sequence":"1"`), []byte(`"sequence":"1","sequence":"1"`), 1)
				case "case":
					raw = bytes.Replace(raw, []byte(`"sequence"`), []byte(`"Sequence"`), 1)
				case "unknown":
					raw = bytes.Replace(raw, []byte(`"sequence":"1"`), []byte(`"sequence":"1","future":true`), 1)
				case "missing sequence":
					raw = bytes.Replace(raw, []byte(`"sequence":"1",`), nil, 1)
				case "null sequence":
					raw = bytes.Replace(raw, []byte(`"sequence":"1"`), []byte(`"sequence":null`), 1)
				case "invalid Unicode":
					raw = bytes.Replace(raw, []byte(`"uid":"original"`), []byte(`"uid":"\ud800"`), 1)
				case "missing creation", "null metadata":
					var v map[string]any
					if err := json.Unmarshal(raw, &v); err != nil {
						t.Fatal(err)
					}
					if mode == "missing creation" {
						delete(v["metadata"].(map[string]any), "creationTimestamp")
					} else {
						v["metadata"] = nil
					}
					raw, _ = json.Marshal(v)
				}
				bad.Payload.Objects[i] = raw
			}
			_ = replication.Seal(bad)
			if _, err := DecodeImage(bad, opts); err == nil {
				t.Fatal("ambiguous original accepted")
			}
		})
	}
}

func TestDecodeImageRejectsBoundsBeforeTypedCarrierValidation(t *testing.T) {
	s, opts := openImageStore(t, filepath.Join(t.TempDir(), sqlite.FileName), replication.Standalone, true)
	initializeImageStore(t, s)
	good := readSourceGeneration(t, s)
	for _, mode := range []string{"head count", "chunk count", "allocation count", "carrier bytes", "aggregate bytes", "foreign full object bytes", "section bytes"} {
		t.Run(mode, func(t *testing.T) {
			bad := cloneSourceGeneration(t, good)
			kind, count := kinds.KindIntegrationSourceHead, kinds.MaxSourceCount+1
			switch mode {
			case "chunk count":
				kind, count = kinds.KindIntegrationSourceChunk, kinds.MaxSourceCount*kinds.MaxSourceChunks+1
			case "allocation count":
				kind, count = kinds.KindIntegrationNameAllocation, kinds.MaxClusterAllocations+1
			case "carrier bytes":
				bad.Payload.Objects = []json.RawMessage{json.RawMessage(`{"kind":"IntegrationSourceChunk","padding":"` + strings.Repeat("x", kinds.MaxObjectBytes) + `"}`)}
			case "aggregate bytes":
				kind, count = kinds.KindIntegrationSourceChunk, MaxImageSourceBytes/(kinds.MaxObjectBytes-128)+1
			case "foreign full object bytes":
				bad.Payload.Objects = []json.RawMessage{json.RawMessage(strings.Repeat("x", (replication.Limits{}).Defaults().MaxObjectBytes+1))}
			case "section bytes":
				bad.Payload.Runtime = []replication.Entry{{Key: "bad", Data: json.RawMessage(strings.Repeat("x", (replication.Limits{}).Defaults().MaxObjectBytes+1))}}
			}
			if mode == "head count" || mode == "chunk count" || mode == "allocation count" || mode == "aggregate bytes" {
				raw := json.RawMessage(`{"kind":"` + kind + `"}`)
				if mode == "aggregate bytes" {
					raw = json.RawMessage(`{"kind":"` + kind + `","padding":"` + strings.Repeat("x", kinds.MaxObjectBytes-256) + `"}`)
				}
				bad.Payload.Objects = make([]json.RawMessage, count)
				for i := range bad.Payload.Objects {
					bad.Payload.Objects[i] = raw
				}
			}
			if _, err := DecodeImage(bad, opts); !errors.Is(err, replication.ErrTooLarge) {
				t.Fatal("typed decode ran before aggregate bounds", err)
			}
		})
	}
}

func TestDecodeImagePermanentUnusedAllocationsAndOrphanChunks(t *testing.T) {
	s, opts := openImageStore(t, filepath.Join(t.TempDir(), sqlite.FileName), replication.Standalone, true)
	initializeImageStore(t, s)
	owner := kinds.Owner{ClusterID: s.Authority().Term.ClusterID, Namespace: "default", Name: "retired-source", UID: "retired-owner"}
	scope := kinds.AllocationScope{ClusterID: owner.ClusterID, ZoneUID: "retired-target", Apex: "lan.test."}
	allocation := &kinds.IntegrationNameAllocation{ObjectMeta: metav1.ObjectMeta{Name: kinds.NameAllocationName(scope, "phone-2")}, Spec: kinds.IntegrationNameAllocationSpec{FormatVersion: 1, Scope: scope, Owner: owner, Label: "phone-2", Base: "phone", ClientID: "omitted", CreatedAt: time.Unix(100000, 0).UTC()}}
	if _, err := s.Apply(t.Context(), allocation, store.ApplyOptions{Manager: store.ManagerSystem, CreateOnly: true}); err != nil {
		t.Fatal(err)
	}
	good := readSourceGeneration(t, s)
	decoded, err := DecodeImage(good, opts)
	if err != nil || len(decoded.Sources) != 0 || len(decoded.Allocations) != 1 {
		t.Fatal("historical reservation was reclaimed/rejected", err)
	}
	// Allocation is allowed before the first poll and after retirement. Chunks
	// have no such independent authority: only a head's exact refs keep them.
	data := []byte(`{}`)
	digest := kinds.Digest(data)
	chunk := &kinds.IntegrationSourceChunk{ObjectMeta: metav1.ObjectMeta{Name: kinds.SourceChunkName(owner, 1, 0, digest)}, Spec: kinds.IntegrationSourceChunkSpec{FormatVersion: 1, Owner: owner, Sequence: 1, Digest: digest, Data: data}}
	if _, err := s.Apply(t.Context(), chunk, store.ApplyOptions{Manager: store.ManagerSystem, CreateOnly: true}); err != nil {
		t.Fatal(err)
	}
	bad := readSourceGeneration(t, s)
	if _, err := DecodeImage(bad, opts); err == nil {
		t.Fatal("orphan chunk acquired authority")
	}
	if _, _, err := s.ReadServingCandidate(t.Context()); err == nil {
		t.Fatal("real owner admitted orphan chunk")
	}
	// Rejection is pure: it neither deletes the row nor renews any timestamps.
	after := readSourceGeneration(t, s)
	if !after.Generation().Same(bad.Generation()) {
		t.Fatal("decode performed GC")
	}
}

func installSourceGeneration(t *testing.T, s *sqlite.Store, image *replication.Image) {
	t.Helper()
	validated, err := s.ValidateImage(t.Context(), *image)
	if err != nil {
		t.Fatal(err)
	}
	a := s.Authority()
	if _, err := s.InstallReplica(t.Context(), validated, replication.InstallOptions{ExpectedTerm: a.Term, ExpectedInstalled: a.Installed}); err != nil {
		t.Fatal(err)
	}
}

func TestDecodeImageActualSQLiteRestartInstallPromotionAndEmptyReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), sqlite.FileName)
	p, opts := openImageStore(t, path, replication.Standalone, true)
	initializeImageStore(t, p)
	first := commitImageSource(t, p, true, "original")
	if _, err := p.EnablePrimary(t.Context(), readSourceGeneration(t, p).Generation()); err != nil {
		t.Fatal(err)
	}
	want := readSourceGeneration(t, p)
	before, err := DecodeImage(want, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	p, opts = openImageStore(t, path, replication.Standalone, true)
	after := readSourceGeneration(t, p)
	if !after.Generation().Same(want.Generation()) {
		t.Fatal("restart changed source generation")
	}
	restored, err := DecodeImage(after, opts)
	if err != nil || !reflect.DeepEqual(before, restored) {
		t.Fatal("restart changed source tokens/expiry/allocations", err)
	}
	fpath := filepath.Join(t.TempDir(), sqlite.FileName)
	f, _ := openImageStore(t, fpath, replication.Follower, true)
	if err := f.Enroll(t.Context(), want.Term); err != nil {
		t.Fatal(err)
	}
	installSourceGeneration(t, f, want)
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	f, fopts := openImageStore(t, fpath, replication.Follower, true)
	fimage := readSourceGeneration(t, f)
	replica, err := DecodeImage(fimage, fopts)
	if err != nil || !reflect.DeepEqual(before, replica) {
		t.Fatal("complete follower install/restart changed evidence", err)
	}
	if err := f.Command(t.Context(), f.Authority().Term, func(context.Context, *sqlite.CommandTx) error { return nil }); !errors.Is(err, replication.ErrReadOnly) {
		t.Fatal("follower acquired source writer", err)
	}
	if _, err := f.Promote(t.Context(), replication.PromoteRequest{ExpectedTerm: fimage.Term, ExpectedInstalled: fimage.Generation(), RequestID: "source-promote"}); err != nil {
		t.Fatal(err)
	}
	promoted, err := DecodeImage(readSourceGeneration(t, f), fopts)
	if err != nil || !reflect.DeepEqual(before.Sources, promoted.Sources) || !reflect.DeepEqual(before.Allocations, promoted.Allocations) {
		t.Fatal("promotion renewed or adopted original source evidence", err)
	}
	doc, err := DecodeSource(promoted.Sources[0])
	if err != nil || !doc.Snapshot.Clients[0].ExpiresAt.Equal(first.Head.Spec.AcceptedAt.Add(time.Hour)) {
		t.Fatal("promotion changed expiry", err)
	}
	state, err := persistence.Decode(readSourceGeneration(t, f).Payload.Runtime, f.Authority().Term.ClusterID, readSourceGeneration(t, f).CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	holder, err := inventory.RestoreState(state.State, func() time.Time { return time.Unix(100000, 0).UTC().Add(72 * time.Hour) })
	if err != nil || holder.Snapshot().Lookup(doc.Snapshot.Clients[0].IPs[0], time.Unix(100000, 0).UTC().Add(72*time.Hour)) != nil {
		t.Fatal("expired original assignment revived", err)
	}
	// Install a complete empty source set. Receivers do not preserve deleted
	// private head/chunk rows from the previous image; owner history remains.
	if err := p.Command(t.Context(), p.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		for _, c := range first.Chunks {
			if err := tx.Delete(ctx, store.KeyOf(c), store.DeleteOptions{Manager: store.ManagerSystem}); err != nil {
				return err
			}
		}
		if err := tx.Delete(ctx, store.KeyOf(first.Head), store.DeleteOptions{Manager: store.ManagerSystem}); err != nil {
			return err
		}
		var a kinds.IntegrationNameAllocation
		if err := tx.Get(ctx, store.Key{Kind: kinds.KindIntegrationNameAllocation, Name: before.Allocations[0].Name}, &a); err != nil {
			return err
		}
		return tx.Delete(ctx, store.KeyOf(&a), store.DeleteOptions{Manager: store.ManagerSystem})
	}); err != nil {
		t.Fatal(err)
	}
	emptyFollower, emptyOpts := openImageStore(t, filepath.Join(t.TempDir(), sqlite.FileName), replication.Follower, true)
	if err := emptyFollower.Enroll(t.Context(), p.Authority().Term); err != nil {
		t.Fatal(err)
	}
	installSourceGeneration(t, emptyFollower, want)
	installSourceGeneration(t, emptyFollower, readSourceGeneration(t, p))
	empty, err := DecodeImage(readSourceGeneration(t, emptyFollower), emptyOpts)
	if err != nil || len(empty.Sources)+len(empty.Allocations) != 0 {
		t.Fatal("empty full source replacement retained old authority", err)
	}
}

func TestDecodeImageMalformedHistoricalRowsCannotInstallServeOrPromote(t *testing.T) {
	p, opts := openImageStore(t, filepath.Join(t.TempDir(), sqlite.FileName), replication.Standalone, true)
	initializeImageStore(t, p)
	commitImageSource(t, p, true, "original")
	good := readSourceGeneration(t, p)
	bad := cloneSourceGeneration(t, good)
	replaceImageCarrier(t, bad, kinds.KindIntegrationNameAllocation, func(v map[string]any) { v["metadata"].(map[string]any)["uid"] = "recreated" })
	if err := replication.Seal(bad); err != nil {
		t.Fatal(err)
	}
	generic, err := replication.Validate(*bad, opts.Validation)
	if err != nil {
		t.Fatal("test must be generically valid", err)
	}
	f, _ := openImageStore(t, filepath.Join(t.TempDir(), sqlite.FileName), replication.Follower, true)
	if err := f.Enroll(t.Context(), bad.Term); err != nil {
		t.Fatal(err)
	}
	before := f.Authority()
	if _, err := f.InstallReplica(t.Context(), generic, replication.InstallOptions{ExpectedTerm: before.Term}); err == nil {
		t.Fatal("generic token bypassed local source association validator")
	}
	if !reflect.DeepEqual(before, f.Authority()) {
		t.Fatal("failed install changed authority")
	}
	// Simulate historical data accepted by a binary with only the core owners.
	path := filepath.Join(t.TempDir(), sqlite.FileName)
	legacy, _ := openImageStore(t, path, replication.Follower, false)
	if err := legacy.Enroll(t.Context(), bad.Term); err != nil {
		t.Fatal(err)
	}
	installSourceGeneration(t, legacy, bad)
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	legacy, _ = openImageStore(t, path, replication.Follower, true)
	before = legacy.Authority()
	if _, _, err := legacy.ReadServingCandidate(t.Context()); err == nil {
		t.Fatal("historical mismatched allocation served")
	}
	if _, err := legacy.Promote(t.Context(), replication.PromoteRequest{ExpectedTerm: bad.Term, ExpectedInstalled: bad.Generation(), RequestID: "source-bad-promote"}); err == nil {
		t.Fatal("historical mismatched allocation promoted")
	}
	if !reflect.DeepEqual(before, legacy.Authority()) {
		t.Fatal("historical rejection changed authority")
	}
	if _, err := legacy.ReadGeneration(t.Context()); err != nil {
		t.Fatal("repair read must remain available", err)
	}
}

func TestDecodeImageActualMultichunkAndUnreferencedHistoricalVersion(t *testing.T) {
	s, opts := openImageStore(t, filepath.Join(t.TempDir(), sqlite.FileName), replication.Standalone, true)
	initializeImageStore(t, s)
	at := time.Unix(100000, 0).UTC()
	owner := kinds.Owner{ClusterID: s.Authority().Term.ClusterID, Namespace: "default", Name: "router", UID: "original"}
	doc := Document{FormatVersion: 1, Owner: owner, Snapshot: host.SourceSnapshot{Complete: true}}
	for i := range 500 {
		doc.Snapshot.Clients = append(doc.Snapshot.Clients, inventory.Client{Source: host.SourceID(owner), ID: "client-" + strconv.Itoa(i), Tags: []string{strings.Repeat("a", 128), strings.Repeat("b", 128), strings.Repeat("c", 128)}, ObservedAt: at, ExpiresAt: at.Add(time.Hour)})
	}
	source, err := EncodeSource(doc, 1, kinds.Digest([]byte("config")), kinds.Digest([]byte("credentials")), at)
	if err != nil || len(source.Chunks) < 2 {
		t.Fatal("multichunk fixture", err)
	}
	if err := s.Command(t.Context(), s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		for _, c := range source.Chunks {
			if _, err := tx.Apply(ctx, c, store.ApplyOptions{Manager: store.ManagerSystem, CreateOnly: true}); err != nil {
				return err
			}
		}
		_, err := tx.Apply(ctx, source.Head, store.ApplyOptions{Manager: store.ManagerSystem, CreateOnly: true})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	image := readSourceGeneration(t, s)
	decoded, err := DecodeImage(image, opts)
	if err != nil || len(decoded.Sources[0].Chunks) != len(source.Chunks) {
		t.Fatal("full-size chunk was rejected", err)
	}
	for i, c := range decoded.Sources[0].Chunks {
		if !bytes.Equal(c.Spec.Data, source.Chunks[i].Spec.Data) {
			t.Fatal("chunk order/content changed")
		}
	}
	// An old, correctly named/digested chunk with no retained head is an orphan.
	historical := source.Chunks[0].DeepCopy()
	historical.Spec.Sequence = 2
	historical.Name = kinds.SourceChunkName(owner, 2, historical.Spec.Index, historical.Spec.Digest)
	historical.UID, historical.ResourceVersion, historical.Generation = "", "", 0
	if _, err := s.Apply(t.Context(), historical, store.ApplyOptions{Manager: store.ManagerSystem, CreateOnly: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeImage(readSourceGeneration(t, s), opts); err == nil {
		t.Fatal("unreferenced historical chunk was retained implicitly")
	}
}

func TestDecodeImageDoesNotAllowCrossOwnerDesiredClaims(t *testing.T) {
	for _, fresh := range []bool{false, true} {
		t.Run(strconv.FormatBool(fresh), func(t *testing.T) {
			s, opts := openImageStore(t, filepath.Join(t.TempDir(), sqlite.FileName), replication.Standalone, true)
			initializeImageStore(t, s)
			first := commitImageSource(t, s, true, "original")
			doc, err := DecodeSource(first)
			if err != nil {
				t.Fatal(err)
			}
			doc.Owner.UID = "other-owner"
			if fresh {
				doc.Objects[0].UID = "fresh-record-uid"
			}
			doc.Snapshot.Desired.SourceID = host.SourceID(doc.Owner)
			for i := range doc.Snapshot.Clients {
				doc.Snapshot.Clients[i].Source = host.SourceID(doc.Owner)
			}
			doc.Allocations[0].Spec.Owner = doc.Owner
			doc.Allocations[0].Spec.Label = "phone-2"
			doc.Snapshot.Desired.RecordSets[0].Spec.Endpoints[0].DNSName = "phone-2.lan.test."
			doc.Objects[0].IntentDigest, err = IntentDigest(doc.Snapshot.Desired.RecordSets[0].Spec)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Command(t.Context(), s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
				a := &kinds.IntegrationNameAllocation{ObjectMeta: metav1.ObjectMeta{Name: kinds.NameAllocationName(doc.Allocations[0].Spec.Scope, "phone-2")}, Spec: doc.Allocations[0].Spec}
				saved, err := tx.Apply(ctx, a, store.ApplyOptions{Manager: store.ManagerSystem, CreateOnly: true})
				if err != nil {
					return err
				}
				doc.Allocations[0].Object.Name, doc.Allocations[0].Object.UID, doc.Allocations[0].Object.ResourceVersion = saved.GetName(), string(saved.GetUID()), saved.GetResourceVersion()
				doc.Allocations[0].Object.IntentDigest, err = IntentDigest(doc.Allocations[0].Spec)
				if err != nil {
					return err
				}
				second, err := EncodeSource(doc, 1, kinds.Digest([]byte("config")), kinds.Digest([]byte("credentials")), first.Head.Spec.AcceptedAt)
				if err != nil {
					return err
				}
				for _, c := range second.Chunks {
					if _, err := tx.Apply(ctx, c, store.ApplyOptions{Manager: store.ManagerSystem, CreateOnly: true}); err != nil {
						return err
					}
				}
				_, err = tx.Apply(ctx, second.Head, store.ApplyOptions{Manager: store.ManagerSystem, CreateOnly: true})
				return err
			}); err != nil {
				t.Fatal(err)
			}
			_, err = DecodeImage(readSourceGeneration(t, s), opts)
			if fresh && err != nil {
				t.Fatal("retained evidence for a different UID at same object key rejected", err)
			}
			if !fresh && err == nil {
				t.Fatal("different source owner claimed the original desired UID")
			}
		})
	}
}

func TestDecodeImagePreflightsWholeImageEscaping(t *testing.T) {
	s, opts := openImageStore(t, filepath.Join(t.TempDir(), sqlite.FileName), replication.Standalone, true)
	initializeImageStore(t, s)
	bad := readSourceGeneration(t, s)
	opts.Validation.Limits.MaxBytes = 2 << 20
	// This incoming RawMessage is <1MiB, but marshaling literal HTML into the
	// full image would exceed 2MiB before generic receiver validation starts.
	bad.Payload.Objects = []json.RawMessage{json.RawMessage(`{"kind":"Unregistered","value":"` + strings.Repeat("<", 512<<10) + `"}`)}
	if _, err := DecodeImage(bad, opts); !errors.Is(err, replication.ErrTooLarge) {
		t.Fatal("whole-image escaped allocation was not bounded", err)
	}
}
