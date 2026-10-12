package validator

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// repoRoot is the repository root relative to this package.
const repoRoot = "../../.."

var (
	sharedOnce sync.Once
	sharedV    *Validator
	sharedErr  error
)

// testValidator returns one Validator for the whole test binary; building
// it compiles every CEL program, which is the expensive part.
func testValidator(t testing.TB) *Validator {
	t.Helper()
	sharedOnce.Do(func() { sharedV, sharedErr = New() })
	if sharedErr != nil {
		t.Fatalf("New: %v", sharedErr)
	}
	return sharedV
}

// Sample files under config/samples, by kind.
var sampleFiles = map[store.Kind]string{
	"DNSZone":        "kudurru.io_v1alpha1_dnszone.yaml",
	"DNSRecordSet":   "kudurru.io_v1alpha1_dnsrecordset.yaml",
	"BlockList":      "kudurru.io_v1alpha1_blocklist.yaml",
	"ClientGroup":    "kudurru.io_v1alpha1_clientgroup.yaml",
	"Policy":         "kudurru.io_v1alpha1_policy.yaml",
	"UnblockRequest": "kudurru.io_v1alpha1_unblockrequest.yaml",
}

func readSample(t testing.TB, kind store.Kind) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot, "config", "samples", sampleFiles[kind]))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// newTyped returns an empty typed object of kind.
func newTyped(t testing.TB, kind store.Kind) store.Object {
	t.Helper()
	switch kind {
	case "DNSZone":
		return &v1alpha1.DNSZone{}
	case "DNSRecordSet":
		return &v1alpha1.DNSRecordSet{}
	case "BlockList":
		return &v1alpha1.BlockList{}
	case "ClientGroup":
		return &v1alpha1.ClientGroup{}
	case "Policy":
		return &v1alpha1.Policy{}
	case "UnblockRequest":
		return &v1alpha1.UnblockRequest{}
	}
	t.Fatalf("no Go type for %s", kind)
	return nil
}

// typedSample decodes the config/samples fixture of kind strictly, so a
// sample that drifts from the Go types fails here first.
func typedSample(t testing.TB, kind store.Kind) store.Object {
	t.Helper()
	obj := newTyped(t, kind)
	if err := yaml.UnmarshalStrict(readSample(t, kind), obj); err != nil {
		t.Fatalf("decode sample %s: %v", kind, err)
	}
	return obj
}

func unstructuredSample(t testing.TB, kind store.Kind) *unstructured.Unstructured {
	t.Helper()
	return unstructuredFromYAML(t, string(readSample(t, kind)))
}

func unstructuredFromYAML(t testing.TB, doc string) *unstructured.Unstructured {
	t.Helper()
	m := map[string]any{}
	if err := yaml.Unmarshal([]byte(doc), &m); err != nil {
		t.Fatal(err)
	}
	return &unstructured.Unstructured{Object: m}
}

// stored returns a copy of obj as a driver would hold it: identity stamped.
func stored(obj store.Object) store.Object {
	out := obj.DeepCopyObject().(store.Object)
	out.SetUID("8d7c6f9e-0000-4000-8000-000000000001")
	out.SetResourceVersion("7")
	out.SetGeneration(3)
	out.SetCreationTimestamp(metav1.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC))
	return out
}

// wantInvalid asserts err is a *Error with a cause at field of type typ and
// returns its causes.
func wantInvalid(t *testing.T, err error, fieldPath string, typ metav1.CauseType) []Cause {
	t.Helper()
	if err == nil {
		t.Fatalf("expected rejection at %s (%s), got nil", fieldPath, typ)
	}
	ve, ok := AsError(err)
	if !ok {
		t.Fatalf("expected *validator.Error, got %T: %v", err, err)
	}
	if !ve.HasCause(fieldPath, typ) {
		t.Fatalf("expected cause %s (%s); got:\n%s", fieldPath, typ, err)
	}
	return ve.Causes
}

func ctx() context.Context { return context.Background() }

func isUnknownKind(err error) bool { return errors.Is(err, ErrUnknownKind) }

// equalJSON compares two objects by their JSON encoding.
func equalJSON(t testing.TB, a, b any) bool {
	t.Helper()
	ja, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	jb, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return string(ja) == string(jb)
}
