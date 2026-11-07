package conformance

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
)

type invalidFixture struct {
	name                  string
	base                  func() store.Object
	change                func(store.Object)
	create, status, force bool
	unknown               string // strict decoded-input boundary, not a typed Store field
	field                 string
	cause                 metav1.CauseType
	message               string
	skippedRules          bool
	tooLarge              bool
}

func invalidFixtures() []invalidFixture {
	z := func() store.Object { return zone("rejected", "home.example.") }
	r := func() store.Object {
		return &v1alpha1.DNSRecordSet{
			ObjectMeta: metav1.ObjectMeta{Name: "rejected", Namespace: "apps"},
			Spec:       v1alpha1.DNSRecordSetSpec{ZoneRef: v1alpha1.ObjectRef{Name: "home"}, Endpoints: []v1alpha1.Endpoint{{DNSName: "home.example.", RecordType: "A", Targets: []string{"192.0.2.1"}}}},
		}
	}
	b := func() store.Object {
		return &v1alpha1.BlockList{
			ObjectMeta: metav1.ObjectMeta{Name: "rejected", Namespace: "apps"},
			Spec:       v1alpha1.BlockListSpec{Sources: []v1alpha1.ListSource{{Inline: []string{"ads.example"}}}},
		}
	}
	badType := func(o store.Object) { o.(*v1alpha1.DNSZone).Spec.Type = "master" }
	newApex := func(o store.Object) { o.(*v1alpha1.DNSZone).Spec.ZoneName = "other.example." }
	large := func(o store.Object) {
		entry := strings.Repeat("a", 60) + ".example"
		inline := make([]string, store.MaxObjectBytes/(len(entry)+3)+2048)
		for i := range inline {
			inline[i] = entry
		}
		o.(*v1alpha1.BlockList).Spec.Sources[0].Inline = inline
	}
	invalid := metav1.CauseTypeFieldValueInvalid
	return []invalidFixture{
		{name: "TypedStore/SchemaEnumCreate", base: z, change: badType, create: true, field: "spec.type", cause: metav1.CauseTypeFieldValueNotSupported, message: `Unsupported value: "master": supported values: "primary", "forward", "secondary", "stub", "external", "catalog", "secondaryCatalog"`, skippedRules: true},
		{name: "TypedStore/SchemaPatternCreate", base: z, create: true, change: func(o store.Object) { o.(*v1alpha1.DNSZone).Spec.ZoneName = "no-trailing-dot.example" }, field: "spec.zoneName", cause: invalid, message: `Invalid value: "no-trailing-dot.example": spec.zoneName in body should match '^([a-zA-Z0-9_-]+\.)*$|^\.$'`},
		{name: "TypedStore/SchemaExclusiveSourceCreate", base: b, change: func(o store.Object) { o.(*v1alpha1.BlockList).Spec.Sources[0].URL = "https://example.test/list" }, create: true, field: "spec.sources[0]", cause: invalid, message: "Invalid value: exactly one of url, inline, configMapRef, entries or rpz must be set"},
		{name: "TypedStore/SchemaEntryLengthUpdate", base: b, change: func(o store.Object) {
			o.(*v1alpha1.BlockList).Spec.Sources[0] = v1alpha1.ListSource{Entries: []v1alpha1.ListEntry{{Value: strings.Repeat("a", 1025)}}}
		}, field: "spec.sources[0].entries[0].value", cause: "FieldValueTooLong", message: "Too long: may not be more than 1024 bytes", skippedRules: true},
		{name: "TypedStore/SchemaRequiredTargets", base: r, create: true, change: func(o store.Object) { o.(*v1alpha1.DNSRecordSet).Spec.Endpoints[0].Targets = nil }, field: "spec.endpoints[0].targets", cause: metav1.CauseTypeFieldValueRequired, message: "Required value", skippedRules: true},
		{name: "TypedStore/ImmutableZoneName", base: z, change: newApex, field: "spec.zoneName", cause: invalid, message: `Invalid value: "other.example.": zoneName is immutable`},
		{name: "TypedStore/ImmutableRecordZoneRef", base: r, change: func(o store.Object) { o.(*v1alpha1.DNSRecordSet).Spec.ZoneRef.Name = "other" }, field: "spec.zoneRef", cause: invalid, message: "Invalid value: zoneRef is immutable"},
		{name: "TypedStore/StatusConditionEnum", base: z, status: true, change: func(o store.Object) {
			o.(*v1alpha1.DNSZone).Status.Conditions = []metav1.Condition{{Type: "Ready", Status: "Maybe", Reason: "Testing", LastTransitionTime: metav1.Now()}}
		}, field: "status.conditions[0].status", cause: metav1.CauseTypeFieldValueNotSupported, message: `Unsupported value: "Maybe": supported values: "True", "False", "Unknown"`, skippedRules: true},
		{name: "TypedStore/SizeCreate", base: b, change: large, create: true, tooLarge: true},
		{name: "TypedStore/SizeUpdate", base: b, change: large, tooLarge: true},
		{name: "TypedStore/SizeStatus", base: b, status: true, change: func(o store.Object) {
			o.(*v1alpha1.BlockList).Status.Sources = []v1alpha1.SourceStatus{{Name: "fixture", Error: strings.Repeat("e", store.MaxObjectBytes+128<<10)}}
		}, tooLarge: true},
		{name: "TypedStore/ForceSchemaEnum", base: z, change: badType, force: true, field: "spec.type", cause: metav1.CauseTypeFieldValueNotSupported, message: `Unsupported value: "master": supported values: "primary", "forward", "secondary", "stub", "external", "catalog", "secondaryCatalog"`, skippedRules: true},
		{name: "TypedStore/ForceImmutableZoneName", base: z, change: newApex, force: true, field: "spec.zoneName", cause: invalid, message: `Invalid value: "other.example.": zoneName is immutable`},
		{name: "TypedStore/ForceSize", base: b, change: large, force: true, tooLarge: true},
		{name: "DecodedInput/UnknownSpecField", base: z, create: true, unknown: "spec.ttl", field: "spec.ttl", cause: validator.CauseUnknownField, message: "field not declared in schema"},
		{name: "DecodedInput/UnknownTopLevelField", base: z, create: true, unknown: "extra", field: "extra", cause: validator.CauseUnknownField, message: "field not declared in schema"},
	}
}

// RunRejections exercises production-driver admission, including rejected
// ownership takeovers. It is separate from Run because the memory reference
// store intentionally has no schema validator. Unknown fields only exist at
// the shared strict decoded-input boundary: Store.Apply accepts registered Go
// types, not arbitrary unstructured objects. Those cases explicitly stop at
// ValidateUnstructured before dispatch, as API/file callers must do.
// durableState supplies effects hidden by Store.List: SQLite feed/audit revision
// and Kubernetes SSA managedFields. Only fixture objects are inspected.
func RunRejections(t *testing.T, factory func(*testing.T) store.Store, durableState func(*testing.T, store.Store, store.Key) any) {
	t.Helper()
	v, err := validator.New()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range invalidFixtures() {
		t.Run(tc.name, func(t *testing.T) { testRejection(t, factory(t), v, tc, durableState) })
	}
}

func testRejection(t *testing.T, s store.Store, v *validator.Validator, tc invalidFixture, durableState func(*testing.T, store.Store, store.Key) any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	obj := tc.base()
	kind := store.KindOf(obj)
	if !tc.create {
		out, err := s.Apply(ctx, obj, store.ApplyOptions{Manager: store.ManagerFile("rejections.yaml")})
		if err != nil {
			t.Fatal(err)
		}
		obj = out
	}
	before := rejectionObjects(ctx, t, s, kind)
	durableBefore := durableState(t, s, store.KeyOf(obj))
	w, err := s.Watch(ctx, kind, store.WatchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Stop()
	for ev := range w.ResultChan() {
		if ev.Type == store.Bookmark {
			break
		}
	}

	if tc.change != nil {
		tc.change(obj)
	}
	var reject error
	switch {
	case tc.unknown != "":
		data, err := json.Marshal(obj)
		if err != nil {
			t.Fatal(err)
		}
		u := &unstructured.Unstructured{}
		if err := json.Unmarshal(data, &u.Object); err != nil {
			t.Fatal(err)
		}
		u.SetGroupVersionKind(v1alpha1.GroupVersion.WithKind(string(kind)))
		if err := unstructured.SetNestedField(u.Object, true, strings.Split(tc.unknown, ".")...); err != nil {
			t.Fatal(err)
		}
		reject = v.ValidateUnstructured(ctx, u, nil)
	case tc.status:
		reject = s.UpdateStatus(ctx, obj)
	default:
		manager := store.ManagerFile("rejections.yaml")
		if tc.force {
			manager = store.ManagerAPI
		}
		out, err := s.Apply(ctx, obj, store.ApplyOptions{Manager: manager, Force: tc.force})
		if out != nil {
			t.Fatal("rejected Apply returned an object")
		}
		reject = err
	}
	// Check effects even when the error representation is wrong, so a type
	// mismatch cannot conceal a durable ownership change.
	after := rejectionObjects(ctx, t, s, kind)
	if !reflect.DeepEqual(durableBefore, durableState(t, s, store.KeyOf(obj))) {
		t.Error("rejected input changed driver-specific durable state")
	}
	if !reflect.DeepEqual(before, after) {
		t.Error("rejected input changed durable objects, resourceVersions, status or ownership")
	}
	// A successful sentinel write on the same kind is an ordered watch
	// barrier, avoiding a sleep-based assertion that no rejection event exists.
	sentinel := tc.base()
	sentinel.SetName("barrier")
	if _, err := s.Apply(ctx, sentinel, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case ev, ok := <-w.ResultChan():
			if !ok {
				t.Fatal("watch closed before sentinel")
			}
			if ev.Type == store.Bookmark {
				continue
			}
			if ev.Type != store.Added || ev.Object == nil || ev.Object.GetName() != "barrier" {
				t.Error("rejected input emitted a watch event")
				continue
			}
			checkRejectionError(t, reject, tc, kind, obj.GetName(), obj.GetNamespace())
			return
		case <-ctx.Done():
			t.Fatal("watch did not reach sentinel")
		}
	}
}

func rejectionObjects(ctx context.Context, t *testing.T, s store.Store, kind store.Kind) []store.Object {
	t.Helper()
	list, err := s.List(ctx, kind, store.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return list.Items
}

func checkRejectionError(t *testing.T, err error, tc invalidFixture, kind store.Kind, name, namespace string) {
	t.Helper()
	if tc.tooLarge {
		if !errors.Is(err, store.ErrTooLarge) {
			t.Fatalf("size rejection: want ErrTooLarge, got %v", err)
		}
		return
	}
	e, ok := validator.AsError(err)
	if !ok {
		t.Fatalf("want structured *validator.Error, got %T: %v", err, err)
	}
	if e.Group != v1alpha1.GroupVersion.Group || e.Kind != string(kind) || e.Name != name || e.Namespace != namespace || e.Key() != "invalid" || e.HTTPStatus() != http.StatusBadRequest {
		t.Fatalf("incorrect validation identity/envelope: %v", e)
	}
	if !reflect.DeepEqual(e.Fields(), []string{tc.field}) || e.Hint() != "fix the listed fields and retry: "+tc.field {
		t.Fatalf("structured field/hint mismatch: %v", e)
	}
	st := e.Status()
	want := []metav1.StatusCause{{Field: tc.field, Type: tc.cause, Message: tc.message}}
	if tc.skippedRules {
		want = append(want, metav1.StatusCause{Field: "<nil>", Type: metav1.CauseTypeFieldValueInvalid,
			Message: "Invalid value: null: some validation rules were not checked because the object was invalid; correct the existing errors to complete validation"})
	}
	if st.Code != http.StatusUnprocessableEntity || st.Reason != metav1.StatusReasonInvalid || !reflect.DeepEqual(st.Details.Causes, want) {
		t.Fatalf("structured rejection differs: got %+v; want Invalid/422 with %+v", st, want)
	}
}
