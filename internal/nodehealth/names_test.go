package nodehealth

import (
	"strings"
	"testing"
)

func TestTargetIdentityAndReservedNamespace(t *testing.T) {
	for _, owner := range []string{"node-a", "NODE-A", strings.Repeat("long owner . /", 500), "日本語"} {
		name := Target(owner)
		if len(name) > 255 || len(strings.Split(name, ".")[0]) != 52 || !Matches(strings.ToUpper(name), owner) || !Reserved(name) || !IsName(name) {
			t.Fatal("target lost bounded exact owner binding")
		}
	}
	if Target("") != "" || Matches(Target("node-a"), "") || Matches(Target("node-a"), "NODE-A") {
		t.Fatal("empty or case-distinct owner accepted")
	}
	for _, name := range []string{Namespace, "malformed." + Namespace, "x.v2." + Namespace} {
		if !Reserved(name) || !IsName(name) {
			t.Fatal("reserved malformed/future name escaped")
		}
	}
	for _, name := range []string{"x." + Namespace + "evil.", "notnode.health.kudurru.internal.", "example."} {
		if Reserved(name) {
			t.Fatal("ordinary name captured by reserved namespace")
		}
	}
}
