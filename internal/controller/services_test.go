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

// TestPolicyBlockedCategories: unknown blockedCategories IDs make the
// Policy Degraded{UnknownCategory}; an unknown service as well makes it
// UnknownService, and allowedServices IDs are checked like services.
func TestPolicyBlockedCategories(t *testing.T) {
	var mu sync.Mutex
	cats := map[string]bool{"ai": true}
	changed := make(chan struct{}, 1)
	e := newEnv(t, func(o *Options) {
		o.KnownService = func(id string) bool { return id == "chatgpt" }
		o.KnownCategory = func(id string) bool { mu.Lock(); defer mu.Unlock(); return cats[id] }
		o.ServicesChanged = changed
	})
	e.start()
	e.apply(&v1alpha1.ClientGroup{ObjectMeta: metav1.ObjectMeta{Namespace: "home", Name: "kids"}})
	pol := &v1alpha1.Policy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "home", Name: "kids"},
		Spec:       v1alpha1.PolicySpec{ClientGroupRefs: []string{"kids"}, BlockedCategories: []string{"ai", "retro"}, AllowedServices: []string{"chatgpt"}},
	}
	e.apply(pol)
	e.eventually("unknown category", func() bool {
		return cond(e.policy("home", "kids").Status.Conditions, v1alpha1.ConditionDegraded) == "True/UnknownCategory"
	})
	if msg := condition(e.policy("home", "kids").Status.Conditions, v1alpha1.ConditionDegraded).Message; !strings.Contains(msg, `category "retro" (blockedCategories)`) || strings.Contains(msg, `"ai"`) {
		t.Errorf("message %q", msg)
	}
	pol = e.policy("home", "kids").DeepCopy()
	pol.Spec.AllowedServices = []string{"chatgpt", "nothing"}
	e.apply(pol)
	e.eventually("unknown allowed service", func() bool {
		c := condition(e.policy("home", "kids").Status.Conditions, v1alpha1.ConditionDegraded)
		return c != nil && c.Reason == v1alpha1.ReasonUnknownService && strings.Contains(c.Message, `service "nothing" (allowedServices)`)
	})
	pol = e.policy("home", "kids").DeepCopy()
	pol.Spec.AllowedServices = nil
	e.apply(pol)
	mu.Lock()
	cats["retro"] = true
	mu.Unlock()
	changed <- struct{}{}
	e.eventually("resolved by the catalogue update", func() bool {
		return cond(e.policy("home", "kids").Status.Conditions, v1alpha1.ConditionDegraded) == "False/AsExpected"
	})
}
