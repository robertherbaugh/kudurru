package policy

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
)

// ScheduleState describes a Policy schedule at one instant, for the
// policy page (GET /policies/{namespace}/{name}/state).
type ScheduleState struct {
	// Active reports whether the schedule allows the policy at the
	// instant.
	Active bool
	// NextChange is the next minute at which Active flips; zero when it
	// does not flip within the next eight days (an every-day, all-day
	// window, or no selected weekday ever reached).
	NextChange time.Time
	// Summary is a one-line description: "Mon-Fri 08:00-17:00
	// (Europe/Berlin)".
	Summary string
}

// scheduleHorizon bounds the search for the next change: one week plus a
// day covers every weekly window.
const scheduleHorizon = 8 * 24 * time.Hour

// DescribeSchedule evaluates s at now exactly as the query path does. An
// invalid schedule returns the error; the Index then treats the policy as
// always active (ReasonInvalidSchedule).
func DescribeSchedule(s *v1alpha1.Schedule, now time.Time) (ScheduleState, error) {
	c, err := compileSchedule(s)
	if err != nil {
		return ScheduleState{}, err
	}
	st := ScheduleState{Active: c.active(now), Summary: c.summary()}
	t := now.Truncate(time.Minute).Add(time.Minute)
	for end := now.Add(scheduleHorizon); t.Before(end); t = t.Add(time.Minute) {
		if c.active(t) != st.Active {
			st.NextChange = t.UTC()
			break
		}
	}
	return st, nil
}

var shortDays = [...]string{"Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"}

// summary renders the schedule: weekdays as Mon..Sun runs, the window in
// local time and the zone.
func (s *schedule) summary() string {
	days := "every day"
	if s.days != everyDay {
		// Monday-first order; runs of three or more become "Mon-Fri".
		order := []time.Weekday{time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday, time.Saturday, time.Sunday}
		var parts []string
		for i := 0; i < len(order); {
			if s.days&(1<<order[i]) == 0 {
				i++
				continue
			}
			j := i
			for j+1 < len(order) && s.days&(1<<order[j+1]) != 0 {
				j++
			}
			switch {
			case j-i >= 2:
				parts = append(parts, shortDays[order[i]]+"-"+shortDays[order[j]])
			case j == i:
				parts = append(parts, shortDays[order[i]])
			default:
				parts = append(parts, shortDays[order[i]], shortDays[order[j]])
			}
			i = j + 1
		}
		days = strings.Join(parts, ", ")
	}
	clock := func(m int) string { return fmt.Sprintf("%02d:%02d", m/60, m%60) }
	window := clock(s.start) + "-" + clock(s.end)
	switch {
	case s.start == 0 && s.end == minutesPerDay, s.start == s.end:
		window = "all day"
	case s.end < s.start:
		window += " (ends the next day)"
	}
	return fmt.Sprintf("%s %s (%s)", days, window, s.loc)
}

// Outranks reports whether policy a wins over b when both apply to a
// client: the higher priority, then the smaller "namespace/name" (the
// order Decision.Policies uses).
func Outranks(a, b *v1alpha1.Policy) bool {
	return cmp.Or(cmp.Compare(b.Spec.Priority, a.Spec.Priority),
		cmp.Compare(objectID(a.Namespace, a.Name), objectID(b.Namespace, b.Name))) < 0
}

// ShadowedBy returns the enabled policies ("namespace/name", in rank
// order) that outrank p for at least one of its client groups: for a
// client of such a group they are the deciding policy and their settings
// (blockMode, blockTTL, upstreamGroup, queryLog, safeSearch, allowOnly)
// win, while the lists of all active policies still apply together.
// Schedules are not considered: a scheduled policy shadows p while it is
// active.
func ShadowedBy(p *v1alpha1.Policy, all []*v1alpha1.Policy) []string {
	var over []*v1alpha1.Policy
	for _, o := range all {
		if o == nil || (o.Namespace == p.Namespace && o.Name == p.Name) || o.Namespace != p.Namespace || v1alpha1.IsDisabled(o) {
			continue
		}
		if !Outranks(o, p) || !slices.ContainsFunc(o.Spec.ClientGroupRefs, func(g string) bool { return slices.Contains(p.Spec.ClientGroupRefs, g) }) {
			continue
		}
		over = append(over, o)
	}
	slices.SortFunc(over, func(a, b *v1alpha1.Policy) int {
		if Outranks(a, b) {
			return -1
		}
		return 1
	})
	out := make([]string, 0, len(over))
	for _, o := range over {
		out = append(out, objectID(o.Namespace, o.Name))
	}
	return out
}
