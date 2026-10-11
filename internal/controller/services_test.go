package controller

import (
	"strings"
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
)

// TestPolicyBlockedServices: blockedServices IDs the catalogue does not
// know make the Policy Degraded{UnknownService}; a missing list reference
// takes precedence and its message names the unknown services too.
func TestPolicyBlockedServices(t *testing.T) {
	var mu sync.Mutex
	known := map[string]bool{"youtube": true, "tiktok": true}
	changed := make(chan struct{}, 1)
	e := newEnv(t, func(o *Options) {
		o.KnownService = func(id string) bool { mu.Lock(); defer mu.Unlock(); return known[id] }
		o.ServicesChanged = changed
	})
	e.start()
	e.apply(&v1alpha1.ClientGroup{ObjectMeta: metav1.ObjectMeta{Namespace: "home", Name: "kids"}})
	pol := &v1alpha1.Policy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "home", Name: "kids"},
		Spec:       v1alpha1.PolicySpec{ClientGroupRefs: []string{"kids"}, BlockedServices: []string{"youtube", "myspace"}},
	}
	e.apply(pol)
	e.eventually("unknown service", func() bool {
		return cond(e.policy("home", "kids").Status.Conditions, v1alpha1.ConditionDegraded) == "True/UnknownService"
	})
	if msg := condition(e.policy("home", "kids").Status.Conditions, v1alpha1.ConditionDegraded).Message; !strings.Contains(msg, `service "myspace" (blockedServices)`) || strings.Contains(msg, "youtube") {
		t.Errorf("message %q", msg)
	}
	if got := cond(e.policy("home", "kids").Status.Conditions, v1alpha1.ConditionReady); got != "True/Serving" {
		t.Errorf("Ready %s", got)
	}

	pol = e.policy("home", "kids").DeepCopy()
	pol.Spec.BlockListRefs = []string{"ads"}
	e.apply(pol)
	e.eventually("missing reference first", func() bool {
		c := condition(e.policy("home", "kids").Status.Conditions, v1alpha1.ConditionDegraded)
		return c != nil && c.Reason == ReasonMissingReference && strings.Contains(c.Message, `BlockList "ads"`) && strings.Contains(c.Message, `"myspace"`)
	})

	pol = e.policy("home", "kids").DeepCopy()
	pol.Spec.BlockListRefs = nil
	e.apply(pol)
	e.eventually("unknown service again", func() bool {
		return cond(e.policy("home", "kids").Status.Conditions, v1alpha1.ConditionDegraded) == "True/UnknownService"
	})
	// A catalogue update that adds the service re-validates the Policy
	// without any object change.
	mu.Lock()
	known["myspace"] = true
	mu.Unlock()
	changed <- struct{}{}
	e.eventually("resolved by the catalogue update", func() bool {
		return cond(e.policy("home", "kids").Status.Conditions, v1alpha1.ConditionDegraded) == "False/AsExpected"
	})
}
