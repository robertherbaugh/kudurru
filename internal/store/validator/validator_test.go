package validator

import (
	"fmt"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// Cause types, shortened for the tables.
const (
	invalid      = metav1.CauseTypeFieldValueInvalid
	required     = metav1.CauseTypeFieldValueRequired
	notSupported = metav1.CauseTypeFieldValueNotSupported
	forbidden    = metav1.CauseTypeForbidden
	tooMany      = metav1.CauseTypeTooMany
	duplicate    = metav1.CauseTypeFieldValueDuplicate
	typeInvalid  = metav1.CauseTypeTypeInvalid
)

func TestNewKnowsEveryKind(t *testing.T) {
	v := testValidator(t)
	got := fmt.Sprint(v.Kinds())
	if want := "[BlockList ClientGroup DHCPScope DNSRecordSet DNSZone Policy UnblockRequest]"; got != want {
		t.Fatalf("Kinds() = %s, want %s", got, want)
	}
	for kind := range sampleFiles {
		gvk, namespaced, ok := v.GroupVersionKind(kind)
		if !ok || gvk.Group != v1alpha1.Group || gvk.Version != v1alpha1.Version {
			t.Fatalf("%s: gvk %v ok=%v", kind, gvk, ok)
		}
		if clusterScoped := kind == "DNSZone" || kind == "DHCPScope"; namespaced == clusterScoped {
			t.Fatalf("%s: namespaced=%v", kind, namespaced)
		}
	}
}

func TestSamplesAreValid(t *testing.T) {
	v := testValidator(t)
	for kind := range sampleFiles {
		t.Run(string(kind), func(t *testing.T) {
			if err := v.Validate(ctx(), typedSample(t, kind), nil); err != nil {
				t.Fatalf("typed sample rejected: %v", err)
			}
			if err := v.ValidateUnstructured(ctx(), unstructuredSample(t, kind), nil); err != nil {
				t.Fatalf("unstructured sample rejected: %v", err)
			}
		})
	}
}

// invalidCase mutates a valid sample into an input the apiserver rejects.
type invalidCase struct {
	name   string
	kind   store.Kind
	mutate func(store.Object)
	field  string
	typ    metav1.CauseType
}

func zoneOf(o store.Object) *v1alpha1.DNSZone       { return o.(*v1alpha1.DNSZone) }
func rrsetOf(o store.Object) *v1alpha1.DNSRecordSet { return o.(*v1alpha1.DNSRecordSet) }
func blOf(o store.Object) *v1alpha1.BlockList       { return o.(*v1alpha1.BlockList) }
func cgOf(o store.Object) *v1alpha1.ClientGroup     { return o.(*v1alpha1.ClientGroup) }
func polOf(o store.Object) *v1alpha1.Policy         { return o.(*v1alpha1.Policy) }

func invalidCases() []invalidCase {
	return []invalidCase{
		// DNSZone: patterns, enums, required, metadata.
		{"zoneName without trailing dot", "DNSZone", func(o store.Object) { zoneOf(o).Spec.ZoneName = "home.example.com" }, "spec.zoneName", invalid},
		{"zoneName with a space", "DNSZone", func(o store.Object) { zoneOf(o).Spec.ZoneName = "bad zone." }, "spec.zoneName", invalid},
		{"zone type enum", "DNSZone", func(o store.Object) { zoneOf(o).Spec.Type = "master" }, "spec.type", notSupported},
		{"zone type empty", "DNSZone", func(o store.Object) { zoneOf(o).Spec.Type = "" }, "spec.type", notSupported},
		{"queryAccess enum", "DNSZone", func(o store.Object) { zoneOf(o).Spec.Policy.QueryAccess = "public" }, "spec.policy.queryAccess", notSupported},
		{"notify enum", "DNSZone", func(o store.Object) { zoneOf(o).Spec.Policy.Notify = "all" }, "spec.policy.notify", notSupported},
		{"secondary transferProtocol enum", "DNSZone", func(o store.Object) {
			zoneOf(o).Spec.Secondary = &v1alpha1.SecondarySpec{Primaries: []string{"10.0.0.1"}, TransferProtocol: "udp"}
		}, "spec.secondary.transferProtocol", notSupported},
		{"secondary primaries required", "DNSZone", func(o store.Object) { zoneOf(o).Spec.Secondary = &v1alpha1.SecondarySpec{} }, "spec.secondary.primaries", required},
		{"stub nameservers required", "DNSZone", func(o store.Object) { zoneOf(o).Spec.Stub = &v1alpha1.StubSpec{} }, "spec.stub.nameservers", required},
		{"dnssec algorithm enum", "DNSZone", func(o store.Object) {
			zoneOf(o).Spec.DNSSEC = &v1alpha1.DNSSECSpec{Enabled: true, Algorithm: "RSASHA1"}
		}, "spec.dnssec.algorithm", notSupported},
		{"publish policy enum", "DNSZone", func(o store.Object) {
			zoneOf(o).Spec.Publish = []v1alpha1.PublishRule{{IntegrationRef: v1alpha1.ObjectRef{Name: "cf"}, Policy: "mirror"}}
		}, "spec.publish[0].policy", notSupported},
		{"name not a DNS subdomain", "DNSZone", func(o store.Object) { o.SetName("Home_Zone") }, "metadata.name", invalid},
		{"name missing", "DNSZone", func(o store.Object) { o.SetName("") }, "metadata.name", required},
		{"namespace on a cluster-scoped kind", "DNSZone", func(o store.Object) { o.SetNamespace("dns") }, "metadata.namespace", forbidden},
		{"label key invalid", "DNSZone", func(o store.Object) { o.SetLabels(map[string]string{"bad key": "x"}) }, "metadata.labels", invalid},

		// DNSRecordSet.
		{"recordType lower case", "DNSRecordSet", func(o store.Object) { rrsetOf(o).Spec.Endpoints[0].RecordType = "a" }, "spec.endpoints[0].recordType", invalid},
		{"recordType empty", "DNSRecordSet", func(o store.Object) { rrsetOf(o).Spec.Endpoints[1].RecordType = "" }, "spec.endpoints[1].recordType", invalid},
		{"targets empty", "DNSRecordSet", func(o store.Object) { rrsetOf(o).Spec.Endpoints[0].Targets = []string{} }, "spec.endpoints[0].targets", invalid},
		{"targets missing", "DNSRecordSet", func(o store.Object) { rrsetOf(o).Spec.Endpoints[0].Targets = nil }, "spec.endpoints[0].targets", required},
		{"endpoints missing", "DNSRecordSet", func(o store.Object) { rrsetOf(o).Spec.Endpoints = nil }, "spec.endpoints", required},
		{"more than 1000 endpoints", "DNSRecordSet", func(o store.Object) {
			eps := make([]v1alpha1.Endpoint, 1001)
			for i := range eps {
				eps[i] = v1alpha1.Endpoint{DNSName: fmt.Sprintf("h%d.home.example.com", i), RecordType: "A", Targets: []string{"10.0.0.1"}}
			}
			rrsetOf(o).Spec.Endpoints = eps
		}, "spec.endpoints", tooMany},
		{"namespace missing on a namespaced kind", "DNSRecordSet", func(o store.Object) { o.SetNamespace("") }, "metadata.namespace", required},

		// BlockList: one-of, enums, minItems.
		{"source with url and inline", "BlockList", func(o store.Object) { blOf(o).Spec.Sources[0].Inline = []string{"ads.example"} }, "spec.sources[0]", invalid},
		{"source with nothing", "BlockList", func(o store.Object) { blOf(o).Spec.Sources[0].URL = "" }, "spec.sources[0]", invalid},
		{"source with all three", "BlockList", func(o store.Object) {
			blOf(o).Spec.Sources[0].Inline = []string{"ads.example"}
			blOf(o).Spec.Sources[0].ConfigMapRef = &v1alpha1.ObjectRef{Name: "lists"}
		}, "spec.sources[0]", invalid},
		{"second source invalid", "BlockList", func(o store.Object) {
			blOf(o).Spec.Sources = append(blOf(o).Spec.Sources, v1alpha1.ListSource{URL: "https://x.example/", ConfigMapRef: &v1alpha1.ObjectRef{Name: "lists"}})
		}, "spec.sources[1]", invalid},
		{"sources empty", "BlockList", func(o store.Object) { blOf(o).Spec.Sources = []v1alpha1.ListSource{} }, "spec.sources", invalid},
		{"sources missing", "BlockList", func(o store.Object) { blOf(o).Spec.Sources = nil }, "spec.sources", required},
		{"format enum", "BlockList", func(o store.Object) { blOf(o).Spec.Sources[0].Format = "json" }, "spec.sources[0].format", notSupported},
		{"mode enum", "BlockList", func(o store.Object) { blOf(o).Spec.Mode = "deny" }, "spec.mode", notSupported},
		{"response enum", "BlockList", func(o store.Object) { blOf(o).Spec.Response = "drop" }, "spec.response", notSupported},

		// ClientGroup: one-of across six selectors.
		{"client with ip and cidr", "ClientGroup", func(o store.Object) { cgOf(o).Spec.Clients[0].IP = "10.0.20.5" }, "spec.clients[0]", invalid},
		{"client with only a name", "ClientGroup", func(o store.Object) { cgOf(o).Spec.Clients[1] = v1alpha1.ClientMatcher{Name: "nobody"} }, "spec.clients[1]", invalid},
		{"client with mac and hostname", "ClientGroup", func(o store.Object) { cgOf(o).Spec.Clients[1].MAC = "aa:bb:cc:dd:ee:ff" }, "spec.clients[1]", invalid},

		// Policy: patterns and enums.
		{"schedule start without leading zero", "Policy", func(o store.Object) { polOf(o).Spec.Schedule.Start = "8:00" }, "spec.schedule.start", invalid},
		{"schedule end 24:00", "Policy", func(o store.Object) { polOf(o).Spec.Schedule.End = "24:00" }, "spec.schedule.end", invalid},
		{"schedule start missing", "Policy", func(o store.Object) { polOf(o).Spec.Schedule.Start = "" }, "spec.schedule.start", invalid},
		{"blockMode enum", "Policy", func(o store.Object) { polOf(o).Spec.BlockMode = "sinkhole" }, "spec.blockMode", notSupported},
	}
}

func TestInvalidVariantsAreRejected(t *testing.T) {
	v := testValidator(t)
	for _, tc := range invalidCases() {
		t.Run(string(tc.kind)+"/"+tc.name, func(t *testing.T) {
			obj := typedSample(t, tc.kind)
			tc.mutate(obj)
			before := obj.DeepCopyObject()
			err := v.Validate(ctx(), obj, nil)
			wantInvalid(t, err, tc.field, tc.typ)
			if !equalJSON(t, before, obj) {
				t.Fatal("a rejected object must be left unchanged")
			}
			// The same rejection on update: ratcheting only forgives fields
			// that did not change, and these all changed. Name and namespace
			// are the store key, so they cannot change on update.
			if tc.field == "metadata.name" || tc.field == "metadata.namespace" {
				return
			}
			old := stored(typedSample(t, tc.kind))
			wantInvalid(t, v.Validate(ctx(), obj.DeepCopyObject().(store.Object), old), tc.field, tc.typ)
		})
	}
}

// TestConcurrentUse exercises one Validator from many goroutines (run the
// suite with -race): drivers share a single instance per process.
func TestConcurrentUse(t *testing.T) {
	v := testValidator(t)
	old := stored(typedSample(t, "DNSZone"))
	t.Run("group", func(t *testing.T) {
		for i := 0; i < 8; i++ {
			t.Run(fmt.Sprint(i), func(t *testing.T) {
				t.Parallel()
				for j := 0; j < 50; j++ {
					for kind := range sampleFiles {
						if err := v.Validate(ctx(), typedSample(t, kind), nil); err != nil {
							t.Error(err)
						}
					}
					upd := zoneOf(typedSample(t, "DNSZone"))
					upd.Spec.ZoneName = "x.example."
					if err := v.Validate(ctx(), upd, old); !IsInvalid(err) {
						t.Errorf("want invalid, got %v", err)
					}
				}
			})
		}
	})
}
