package blockpage

import (
	"context"
	"net/netip"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	engine "github.com/robertherbaugh/kudurru/internal/blocking"
	"github.com/robertherbaugh/kudurru/internal/blocking/parser"
	"github.com/robertherbaugh/kudurru/internal/policy"
)

func testIndex(t *testing.T) *policy.Index {
	t.Helper()
	compile := func(name string, mode engine.Mode, lines ...string) *engine.List {
		l, err := engine.Compile(engine.Spec{Name: name, Mode: mode, Sources: []engine.Source{engine.LinesSource("inline", parser.FormatAuto, lines)}}, nil, engine.CompileOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return l
	}
	snap, err := engine.NewSnapshot(1, compile("default/ads", engine.ModeBlock, "||ads.example^"), compile("default/school", engine.ModeAllow, "||school.example^"))
	if err != nil {
		t.Fatal(err)
	}
	meta := func(name string) metav1.ObjectMeta { return metav1.ObjectMeta{Name: name, Namespace: "default"} }
	cg := func(name, cidr string) *v1alpha1.ClientGroup {
		return &v1alpha1.ClientGroup{ObjectMeta: meta(name), Spec: v1alpha1.ClientGroupSpec{Clients: []v1alpha1.ClientMatcher{{CIDR: cidr}}}}
	}
	src := v1alpha1.ListSource{URL: "https://lists.example/x.txt"}
	return policy.Build(policy.Input{
		ClientGroups: []*v1alpha1.ClientGroup{cg("lan", "10.0.0.0/8"), cg("kids", "10.0.20.0/24")},
		Policies: []*v1alpha1.Policy{
			{ObjectMeta: meta("home"), Spec: v1alpha1.PolicySpec{ClientGroupRefs: []string{"lan"}, BlockListRefs: []string{"ads"}}},
			{ObjectMeta: meta("kids"), Spec: v1alpha1.PolicySpec{ClientGroupRefs: []string{"kids"}, AllowOnly: true, AllowListRefs: []string{"school"}, Priority: 10}},
		},
		BlockLists: []*v1alpha1.BlockList{
			{ObjectMeta: meta("ads"), Spec: v1alpha1.BlockListSpec{Sources: []v1alpha1.ListSource{src}}},
			{ObjectMeta: meta("school"), Spec: v1alpha1.BlockListSpec{Mode: "allow", Sources: []v1alpha1.ListSource{src}}},
		},
		Lists: snap,
	}, policy.Options{})
}

// TestPolicyFinder evaluates the name against the client's current policy:
// list blocks, allow-only blocks, names that pass, unknown clients.
func TestPolicyFinder(t *testing.T) {
	f := PolicyFinder{Source: policy.Static{Idx: testIndex(t)}}
	ctx, now := context.Background(), time.Now()
	r, ok, _ := f.Find(ctx, netip.MustParseAddr("10.0.0.5"), "www.ads.example", now)
	if !ok || r.Kind != KindList || r.List != "default/ads" || r.Rule != "||ads.example^" || r.Policy != "default/home" {
		t.Errorf("list block: %+v %v", r, ok)
	}
	r, ok, _ = f.Find(ctx, netip.MustParseAddr("10.0.20.5"), "games.example", now)
	if !ok || r.Kind != KindPolicy || r.Policy != "default/kids" {
		t.Errorf("allow-only block: %+v %v", r, ok)
	}
	if _, ok, _ := f.Find(ctx, netip.MustParseAddr("10.0.0.5"), "www.example", now); ok {
		t.Error("an unblocked name has no reason")
	}
	if _, ok, _ := f.Find(ctx, netip.MustParseAddr("192.0.2.1"), "www.ads.example", now); ok {
		t.Error("a client without a policy has no reason")
	}
	if got := f.Policy(netip.MustParseAddr("::ffff:10.0.0.5"), now); got != "default/home" {
		t.Errorf("Policy = %q", got)
	}
	if got := (PolicyFinder{}).Policy(netip.MustParseAddr("10.0.0.5"), now); got != "" {
		t.Errorf("no source: %q", got)
	}
}
