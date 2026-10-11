package policy

import (
	"slices"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
)

func TestDescribeSchedule(t *testing.T) {
	// 2026-10-12 is a Monday.
	mon := func(h, m int) time.Time { return time.Date(2026, 10, 12, h, m, 0, 0, time.UTC) }
	for _, tc := range []struct {
		name    string
		s       v1alpha1.Schedule
		now     time.Time
		active  bool
		next    time.Time
		summary string
	}{
		{"weekday hours, inside", v1alpha1.Schedule{Weekdays: []string{"mon", "tue", "wed", "thu", "fri"}, Start: "08:00", End: "17:00"},
			mon(9, 30), true, mon(17, 0), "Mon-Fri 08:00-17:00 (UTC)"},
		{"weekday hours, before", v1alpha1.Schedule{Weekdays: []string{"mon", "tue", "wed", "thu", "fri"}, Start: "08:00", End: "17:00"},
			mon(7, 59), false, mon(8, 0), "Mon-Fri 08:00-17:00 (UTC)"},
		{"weekend from friday evening", v1alpha1.Schedule{Weekdays: []string{"sat", "sun"}, Start: "00:00", End: "24:00"},
			mon(12, 0), false, time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC), "Sat, Sun all day (UTC)"},
		{"across midnight", v1alpha1.Schedule{Start: "21:00", End: "07:00"},
			mon(6, 0), true, mon(7, 0), "every day 21:00-07:00 (ends the next day) (UTC)"},
		{"all day every day", v1alpha1.Schedule{Start: "00:00", End: "00:00"},
			mon(6, 0), true, time.Time{}, "every day all day (UTC)"},
		{"time zone", v1alpha1.Schedule{Weekdays: []string{"mon", "wed", "fri"}, Start: "18:00", End: "20:00", TZ: "Europe/Berlin"},
			mon(16, 30), true, mon(18, 0), "Mon, Wed, Fri 18:00-20:00 (Europe/Berlin)"},
	} {
		st, err := DescribeSchedule(&tc.s, tc.now)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if st.Active != tc.active || !st.NextChange.Equal(tc.next) || st.Summary != tc.summary {
			t.Errorf("%s: active %v next %v summary %q; want %v %v %q", tc.name, st.Active, st.NextChange, st.Summary, tc.active, tc.next, tc.summary)
		}
	}
	if _, err := DescribeSchedule(&v1alpha1.Schedule{Weekdays: []string{"someday"}}, mon(0, 0)); err == nil {
		t.Error("an invalid schedule must fail")
	}
}

func TestShadowedBy(t *testing.T) {
	pol := func(ns, name string, prio int, groups ...string) *v1alpha1.Policy {
		return &v1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Spec: v1alpha1.PolicySpec{Priority: prio, ClientGroupRefs: groups}}
	}
	kids := pol("home", "kids", 10, "kids", "tablets")
	disabled := pol("home", "off", 99, "kids")
	disabled.Annotations = map[string]string{v1alpha1.EnabledAnnotation: "false"}
	all := []*v1alpha1.Policy{
		kids,
		pol("home", "bedtime", 50, "kids"),
		pol("home", "a-tie", 10, "tablets"), // equal priority, smaller name: wins
		pol("home", "z-tie", 10, "tablets"), // equal priority, larger name: loses
		pol("home", "guests", 90, "guests"), // no shared group
		pol("other", "kids", 99, "kids"),    // other namespace
		pol("home", "low", 1, "kids"),
		disabled,
	}
	if got := ShadowedBy(kids, all); !slices.Equal(got, []string{"home/bedtime", "home/a-tie"}) {
		t.Errorf("ShadowedBy = %v", got)
	}
	if got := ShadowedBy(all[1], all); len(got) != 0 {
		t.Errorf("the top policy is shadowed by %v", got)
	}
}
