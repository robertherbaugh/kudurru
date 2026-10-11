package controller

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// TestDisabledCondition: kudurru.io/enabled: "false" turns Ready False and
// Disabled True (reason Disabled) on BlockList, ClientGroup and Policy,
// never Degraded; a Policy names the disabled objects it skips; enabling
// removes the Disabled condition again.
func TestDisabledCondition(t *testing.T) {
	e := newEnv(t)
	e.start()
	meta := func(name, enabled string) metav1.ObjectMeta {
		return metav1.ObjectMeta{Namespace: "home", Name: name, Annotations: map[string]string{v1alpha1.EnabledAnnotation: enabled}}
	}
	bl := func(enabled string) *v1alpha1.BlockList {
		return &v1alpha1.BlockList{ObjectMeta: meta("ads", enabled), Spec: v1alpha1.BlockListSpec{Sources: []v1alpha1.ListSource{{Inline: []string{"ads.example"}}}}}
	}
	cg := func(enabled string) *v1alpha1.ClientGroup {
		return &v1alpha1.ClientGroup{ObjectMeta: meta("kids", enabled), Spec: v1alpha1.ClientGroupSpec{Clients: []v1alpha1.ClientMatcher{{IP: "192.168.1.5"}}}}
	}
	pol := func(enabled string) *v1alpha1.Policy {
		return &v1alpha1.Policy{ObjectMeta: meta("kids", enabled), Spec: v1alpha1.PolicySpec{ClientGroupRefs: []string{"kids"}, BlockListRefs: []string{"ads"}}}
	}
	conds := func(obj store.Object) []metav1.Condition {
		t.Helper()
		switch o := obj.(type) {
		case *v1alpha1.BlockList:
			var out v1alpha1.BlockList
			_ = e.st.Get(context.Background(), store.KeyOf(o), &out)
			return out.Status.Conditions
		case *v1alpha1.ClientGroup:
			var out v1alpha1.ClientGroup
			_ = e.st.Get(context.Background(), store.KeyOf(o), &out)
			return out.Status.Conditions
		case *v1alpha1.Policy:
			return e.policy(o.Namespace, o.Name).Status.Conditions
		}
		return nil
	}
	e.apply(bl("false"))
	e.apply(cg("false"))
	e.apply(pol("true"))
	for _, o := range []store.Object{bl("false"), cg("false")} {
		e.eventually("disabled "+o.GetName(), func() bool {
			c := conds(o)
			return cond(c, v1alpha1.ConditionReady) == "False/Disabled" && cond(c, v1alpha1.ConditionDisabled) == "True/Disabled" &&
				cond(c, v1alpha1.ConditionDegraded) == "False/AsExpected"
		})
	}
	e.eventually("policy names the skipped references", func() bool {
		c := condition(conds(pol("true")), v1alpha1.ConditionReady)
		return c != nil && c.Status == metav1.ConditionTrue && strings.Contains(c.Message, `BlockList "ads"`) && strings.Contains(c.Message, `ClientGroup "kids"`)
	})
	if got := cond(conds(pol("true")), v1alpha1.ConditionDisabled); got != "" {
		t.Fatalf("an enabled policy carries Disabled %s", got)
	}

	// Disabling the policy itself, then enabling everything again.
	e.apply(pol("false"))
	e.eventually("policy disabled", func() bool { return cond(conds(pol("false")), v1alpha1.ConditionDisabled) == "True/Disabled" })
	e.apply(bl("true"))
	e.apply(cg("true"))
	e.apply(pol("true"))
	for _, o := range []store.Object{bl("true"), cg("true"), pol("true")} {
		e.eventually("enabled "+o.GetName(), func() bool {
			c := conds(o)
			return cond(c, v1alpha1.ConditionReady) == "True/Serving" && cond(c, v1alpha1.ConditionDisabled) == ""
		})
	}
	e.eventually("policy without skipped references", func() bool {
		c := condition(conds(pol("true")), v1alpha1.ConditionReady)
		return c != nil && !strings.Contains(c.Message, "disabled")
	})
}
