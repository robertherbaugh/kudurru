package validator

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

var update = flag.Bool("update", false, "rewrite testdata/*.golden")

// apiEnvelope is the shape internal/api writes: {error:{key,message,hint}}.
type apiEnvelope struct {
	Error struct {
		Key     string   `json:"key"`
		Message string   `json:"message"`
		Hint    string   `json:"hint"`
		Fields  []string `json:"fields"`
	} `json:"error"`
}

func envelopeFor(e *Error) apiEnvelope {
	var env apiEnvelope
	env.Error.Key, env.Error.Message, env.Error.Hint, env.Error.Fields = e.Key(), e.Message(), e.Hint(), e.Fields()
	return env
}

// TestErrorGolden pins the user-visible text of representative rejections:
// the message is what kubectl prints for the same input, and the envelope
// is what the API returns.
func TestErrorGolden(t *testing.T) {
	v := testValidator(t)
	var b strings.Builder
	for _, tc := range invalidCases() {
		switch tc.name {
		case "zoneName without trailing dot", "zone type enum", "source with url and inline",
			"client with ip and cidr", "targets missing", "schedule start without leading zero", "name not a DNS subdomain":
		default:
			continue
		}
		obj := typedSample(t, tc.kind)
		tc.mutate(obj)
		ve, ok := AsError(v.Validate(ctx(), obj, nil))
		if !ok {
			t.Fatalf("%s: not rejected", tc.name)
		}
		fmt.Fprintf(&b, "## %s/%s\n%s\n", tc.kind, tc.name, prettyJSON(t, envelopeFor(ve)))
	}
	old := stored(typedSample(t, "DNSZone"))
	upd := typedSample(t, "DNSZone")
	zoneOf(upd).Spec.ZoneName = "lab.example.com."
	ve, _ := AsError(v.Validate(ctx(), upd, old))
	fmt.Fprintf(&b, "## DNSZone/zoneName changed on update\n%s", prettyJSON(t, envelopeFor(ve)))

	golden := filepath.Join("testdata", "errors.golden")
	if *update {
		if err := os.MkdirAll("testdata", 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, []byte(b.String()), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden) //nolint:gosec // fixed path under testdata
	if err != nil {
		t.Fatalf("%v (run go test -run TestErrorGolden -update)", err)
	}
	if got := b.String(); got != string(want) {
		t.Fatalf("golden mismatch (run with -update to accept):\n%s", got)
	}
}

func prettyJSON(t *testing.T, v any) string {
	t.Helper()
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestErrorMatchesApiserver(t *testing.T) {
	gk := schema.GroupKind{Group: "kudurru.io", Kind: "DNSZone"}
	errs := field.ErrorList{
		field.Invalid(field.NewPath("spec", "zoneName"), "home", "should match '^x$'"),
		field.NotSupported(field.NewPath("spec", "type"), "master", []string{"primary", "forward"}),
	}
	local := newError(gk, "", "home", errs)
	remote := apierrors.NewInvalid(gk, "home", errs)

	if local.Error() != remote.Error() {
		t.Fatalf("message differs:\nlocal  %s\nremote %s", local.Error(), remote.Error())
	}
	st := local.Status()
	if st.Code != remote.ErrStatus.Code || st.Reason != remote.ErrStatus.Reason || st.Message != remote.ErrStatus.Message {
		t.Fatalf("status differs: %+v vs %+v", st, remote.ErrStatus)
	}
	if !equality.Semantic.DeepEqual(st.Details, remote.ErrStatus.Details) {
		t.Fatalf("details differ: %+v vs %+v", st.Details, remote.ErrStatus.Details)
	}

	// The kube driver converts the apiserver's error into the same type.
	conv, ok := FromStatusError(fmt.Errorf("apply: %w", remote))
	if !ok {
		t.Fatal("FromStatusError did not recognise a 422")
	}
	if conv.Error() != local.Error() || len(conv.Causes) != len(local.Causes) {
		t.Fatalf("converted %v, local %v", conv, local)
	}
	for i := range conv.Causes {
		if conv.Causes[i].Field != local.Causes[i].Field || conv.Causes[i].Type != local.Causes[i].Type || conv.Causes[i].Message != local.Causes[i].Message {
			t.Fatalf("cause %d: %+v vs %+v", i, conv.Causes[i], local.Causes[i])
		}
	}
	if !IsInvalid(remote) || !IsInvalid(local) || IsInvalid(errors.New("x")) {
		t.Fatal("IsInvalid")
	}
	if _, ok := FromStatusError(apierrors.NewNotFound(schema.GroupResource{Resource: "dnszones"}, "x")); ok {
		t.Fatal("NotFound is not a validation error")
	}
	if local.HTTPStatus() != http.StatusBadRequest || local.Key() != "invalid" {
		t.Fatalf("HTTP mapping: %d %s", local.HTTPStatus(), local.Key())
	}
	if want := "fix the listed fields and retry: spec.zoneName, spec.type"; local.Hint() != want {
		t.Fatalf("hint = %q", local.Hint())
	}
}

func TestUnknownFieldsError(t *testing.T) {
	e := newUnknownFieldsError(schema.GroupKind{Group: "kudurru.io", Kind: "Policy"}, "dns", "kids", []string{"spec.foo", "spec.bar"})
	want := `Policy.kudurru.io "kids" is invalid: strict decoding error: unknown field "spec.foo", unknown field "spec.bar"`
	if e.Error() != want {
		t.Fatalf("got %s", e.Error())
	}
	if !e.HasCause("spec.bar", CauseUnknownField) {
		t.Fatal("cause missing")
	}
}
