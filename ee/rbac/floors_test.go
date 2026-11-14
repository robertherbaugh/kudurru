package rbac

import (
	"context"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/rbac"
)

// A restarted replica (fresh memory) or one that never observed a removal
// must not honour an earlier session's frozen grant after the binding is
// granted again. Durable floors close that gap; before they load, frozen
// grants are refused while live bindings keep working.
func TestBindingSourceDurableFloorsSurviveRestartAndRegrant(t *testing.T) {
	p := mustPerms(t)
	now := time.Unix(1_800_000_000, 0)
	clock := func() time.Time { return now }
	ops := rbac.Binding{Role: "operator", Scope: rbac.Scope{Kind: rbac.ScopeCluster}, Subjects: []rbac.Subject{{Kind: rbac.SubjectGroup, Name: "corp:/ops"}}}
	vic := rbac.Binding{Role: "viewer", Scope: rbac.Scope{Kind: rbac.ScopeCluster}, Subjects: []rbac.Subject{{Kind: rbac.SubjectUser, Name: "vic"}}}
	toggle := rbac.Attributes{Resource: "blocking", Verb: "toggle", Scope: rbac.Scope{Kind: rbac.ScopeCluster}}
	ctx := context.Background()
	sess := rbac.Principal{Name: "corp:carol", Grants: []rbac.Grant{{Role: "operator", Scope: rbac.Scope{Kind: rbac.ScopeCluster}}}, GrantsIssuedAt: now.Add(-time.Minute)}

	// Replica A observes the removal and records its floor.
	a, err := NewBindingSource(p, nil, SourceOptions{Now: clock, DurableFloors: true})
	if err != nil {
		t.Fatal(err)
	}
	a.SetObjects(map[string]rbac.Binding{"dns/ops": ops, "dns/vic": vic})
	if d, _ := a.Authorize(ctx, sess, toggle); d.Allowed {
		t.Fatal("frozen grant honoured before durable floors loaded")
	}
	if d, _ := a.Authorize(ctx, rbac.Principal{Name: "vic"}, rbac.Attributes{Resource: "zones", Verb: "read"}); !d.Allowed {
		t.Fatal("live binding refused while floors load")
	}
	a.MergeFloors(nil)
	if d, _ := a.Authorize(ctx, sess, toggle); !d.Allowed {
		t.Fatal("frozen grant refused after floors loaded")
	}
	var persisted map[rbac.Grant]time.Time
	a.OnRevoke(func([]Revocation) { persisted = a.Floors() })
	now = now.Add(time.Minute)
	a.SetObjects(map[string]rbac.Binding{"dns/vic": vic})
	if len(persisted) != 1 {
		t.Fatal("removal produced no floor to persist", persisted)
	}

	// Replica B restarts (or never saw the change) and the binding is granted
	// again before it starts. Without floors it would revive the session.
	now = now.Add(time.Minute)
	b, err := NewBindingSource(p, nil, SourceOptions{Now: clock, DurableFloors: true})
	if err != nil {
		t.Fatal(err)
	}
	b.SetObjects(map[string]rbac.Binding{"dns/ops": ops, "dns/vic": vic})
	b.MergeFloors(persisted)
	if d, _ := b.Authorize(ctx, sess, toggle); d.Allowed {
		t.Fatal("restarted replica revived a revoked frozen grant after regrant")
	}
	fresh := sess
	fresh.GrantsIssuedAt = now
	if d, _ := b.Authorize(ctx, fresh, toggle); !d.Allowed {
		t.Fatal("a login after the regrant was refused")
	}
	// Floors only rise: merging an older floor cannot restore the grant.
	b.MergeFloors(map[rbac.Grant]time.Time{{Role: "operator", Scope: rbac.Scope{Kind: rbac.ScopeCluster}}: sess.GrantsIssuedAt.Add(-time.Hour)})
	if d, _ := b.Authorize(ctx, sess, toggle); d.Allowed {
		t.Fatal("an older floor lowered the revocation")
	}

	// Without DurableFloors the documented in-memory behaviour is unchanged.
	legacy, err := NewBindingSource(p, nil, SourceOptions{Now: clock})
	if err != nil {
		t.Fatal(err)
	}
	legacy.SetObjects(map[string]rbac.Binding{"dns/ops": ops})
	if d, _ := legacy.Authorize(ctx, sess, toggle); !d.Allowed || !legacy.FloorsLoaded() {
		t.Fatal("legacy source changed behaviour")
	}
}
