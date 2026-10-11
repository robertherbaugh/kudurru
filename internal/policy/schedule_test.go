package policy

import (
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
)

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Skipf("time zone %s unavailable: %v", name, err)
	}
	return loc
}

func TestScheduleMatrix(t *testing.T) {
	berlin := mustLoc(t, "Europe/Berlin")
	ny := mustLoc(t, "America/New_York")
	school := &v1alpha1.Schedule{Weekdays: []string{"mon", "tue", "wed", "thu", "fri"}, Start: "08:00", End: "20:00", TZ: "Europe/Berlin"}
	night := &v1alpha1.Schedule{Weekdays: []string{"fri", "sat"}, Start: "22:00", End: "06:00", TZ: "America/New_York"}
	allDay := &v1alpha1.Schedule{Weekdays: []string{"Sunday"}, Start: "07:30", End: "07:30", TZ: "UTC"}
	weekend := &v1alpha1.Schedule{Weekdays: []string{"sat", "sun"}}
	untilMidnight := &v1alpha1.Schedule{Start: "18:00", End: "24:00"}

	// 2026-10-12 is a Monday.
	cases := []struct {
		name string
		s    *v1alpha1.Schedule
		at   time.Time
		want bool
	}{
		{"school: monday 08:00 Berlin starts", school, time.Date(2026, 10, 12, 8, 0, 0, 0, berlin), true},
		{"school: monday 07:59 Berlin before", school, time.Date(2026, 10, 12, 7, 59, 0, 0, berlin), false},
		{"school: monday 19:59 Berlin inside", school, time.Date(2026, 10, 12, 19, 59, 0, 0, berlin), true},
		{"school: monday 20:00 Berlin end is exclusive", school, time.Date(2026, 10, 12, 20, 0, 0, 0, berlin), false},
		{"school: saturday noon Berlin", school, time.Date(2026, 10, 17, 12, 0, 0, 0, berlin), false},
		{"school: 06:30 UTC is 08:30 CEST", school, time.Date(2026, 10, 12, 6, 30, 0, 0, time.UTC), true},
		{"school: 06:30 UTC in winter is 07:30 CET", school, time.Date(2026, 12, 7, 6, 30, 0, 0, time.UTC), false},
		{"school: sunday 23:30 UTC is monday 01:30 Berlin", school, time.Date(2026, 10, 11, 23, 30, 0, 0, time.UTC), false},
		{"night: friday 23:00 NY", night, time.Date(2026, 10, 16, 23, 0, 0, 0, ny), true},
		{"night: saturday 05:59 NY belongs to friday's window", night, time.Date(2026, 10, 17, 5, 59, 0, 0, ny), true},
		{"night: saturday 06:00 NY ends", night, time.Date(2026, 10, 17, 6, 0, 0, 0, ny), false},
		{"night: sunday 03:00 NY belongs to saturday's window", night, time.Date(2026, 10, 18, 3, 0, 0, 0, ny), true},
		{"night: monday 03:00 NY (sunday not selected)", night, time.Date(2026, 10, 19, 3, 0, 0, 0, ny), false},
		{"night: friday 03:00 NY (thursday not selected)", night, time.Date(2026, 10, 16, 3, 0, 0, 0, ny), false},
		{"night: saturday 03:00 UTC is friday 23:00 NY", night, time.Date(2026, 10, 17, 3, 0, 0, 0, time.UTC), true},
		{"24h: sunday 07:30", allDay, time.Date(2026, 10, 18, 7, 30, 0, 0, time.UTC), true},
		{"24h: monday 07:29 still sunday's window", allDay, time.Date(2026, 10, 19, 7, 29, 0, 0, time.UTC), true},
		{"24h: monday 07:30 over", allDay, time.Date(2026, 10, 19, 7, 30, 0, 0, time.UTC), false},
		{"24h: sunday 07:29 is saturday's (not selected)", allDay, time.Date(2026, 10, 18, 7, 29, 0, 0, time.UTC), false},
		{"weekend all day: saturday 00:00", weekend, time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC), true},
		{"weekend all day: sunday 23:59", weekend, time.Date(2026, 10, 18, 23, 59, 0, 0, time.UTC), true},
		{"weekend all day: monday 00:00", weekend, time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC), false},
		{"until 24:00: 23:59", untilMidnight, time.Date(2026, 10, 19, 23, 59, 0, 0, time.UTC), true},
		{"until 24:00: 00:00", untilMidnight, time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, err := compileSchedule(tc.s)
			if err != nil {
				t.Fatal(err)
			}
			if got := s.active(tc.at); got != tc.want {
				t.Errorf("active(%s) = %v, want %v", tc.at.Format(time.RFC3339), got, tc.want)
			}
		})
	}
}

func TestScheduleInvalid(t *testing.T) {
	for _, s := range []*v1alpha1.Schedule{
		{Weekdays: []string{"someday"}},
		{Start: "25:00", End: "06:00"},
		{Start: "8", End: "09:00"},
		{Start: "08:00", End: "09:60"},
		{Start: "24:00", End: "06:00"},
		{TZ: "Mars/Olympus_Mons"},
	} {
		if _, err := compileSchedule(s); err == nil {
			t.Errorf("%+v: expected an error", s)
		}
	}
}
