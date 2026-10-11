package policy

import (
	"net/netip"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/blocking"
	"github.com/robertherbaugh/kudurru/internal/blocking/parser"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
)

const ns = "default"

func group(namespace, name string, clients ...v1alpha1.ClientMatcher) *v1alpha1.ClientGroup {
	return &v1alpha1.ClientGroup{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       v1alpha1.ClientGroupSpec{Clients: clients},
	}
}

func pol(namespace, name string, spec v1alpha1.PolicySpec) *v1alpha1.Policy {
	return &v1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}, Spec: spec}
}

func blockList(namespace, name string, mode v1alpha1.ListMode, inline ...string) *v1alpha1.BlockList {
	return &v1alpha1.BlockList{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       v1alpha1.BlockListSpec{Mode: mode, Sources: []v1alpha1.ListSource{{Inline: inline}}},
	}
}

// compile compiles inline lists into a snapshot; names are the snapshot
// names.
func compile(t testing.TB, lists map[string][]string, modes map[string]blocking.Mode) *blocking.Snapshot {
	t.Helper()
	var compiled []*blocking.List
	for name, lines := range lists {
		mode := modes[name]
		if mode == "" {
			mode = blocking.ModeBlock
		}
		l, err := blocking.Compile(blocking.Spec{Name: name, Mode: mode, Sources: []blocking.Source{
			blocking.LinesSource("inline", parser.FormatAuto, lines),
		}}, nil, blocking.CompileOptions{})
		if err != nil {
			t.Fatal(err)
		}
		compiled = append(compiled, l)
	}
	snap, err := blocking.NewSnapshot(1, compiled...)
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

func client(addr string) pipeline.Client {
	return pipeline.Client{Addr: netip.AddrPortFrom(netip.MustParseAddr(addr), 53000)}
}

func dur(d time.Duration) metav1.Duration { return metav1.Duration{Duration: d} }

func ptr[T any](v T) *T { return &v }

// staticHosts is a HostnameResolver backed by a map.
type staticHosts map[netip.Addr][]string

func (h staticHosts) Hostnames(a netip.Addr) []string { return h[a] }
