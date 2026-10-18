package policy

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
)

const (
	minutesPerDay = 24 * 60
	everyDay      = 0x7f
)

// schedule is a compiled v1alpha1.Schedule. Times are minutes since local
// midnight; the window is [start, end) on the selected weekdays, crossing
// midnight when end <= start.
type schedule struct {
	days       uint8 // bit (1 << time.Weekday)
	start, end int
	loc        *time.Location
}

var weekdayNames = map[string]time.Weekday{
	"sun": time.Sunday, "sunday": time.Sunday,
	"mon": time.Monday, "monday": time.Monday,
	"tue": time.Tuesday, "tues": time.Tuesday, "tuesday": time.Tuesday,
	"wed": time.Wednesday, "wednesday": time.Wednesday,
	"thu": time.Thursday, "thur": time.Thursday, "thurs": time.Thursday, "thursday": time.Thursday,
	"fri": time.Friday, "friday": time.Friday,
	"sat": time.Saturday, "saturday": time.Saturday,
}

// compileSchedule validates s. An empty weekday list means every day, an
// empty start midnight, an empty end the end of the day, an empty tz UTC.
func compileSchedule(s *v1alpha1.Schedule) (*schedule, error) {
	out := &schedule{loc: time.UTC, end: minutesPerDay}
	for _, d := range s.Weekdays {
		wd, ok := weekdayNames[strings.ToLower(strings.TrimSpace(d))]
		if !ok {
			return nil, fmt.Errorf("unknown weekday %q (mon..sun)", d)
		}
		out.days |= 1 << wd
	}
	if out.days == 0 {
		out.days = everyDay
	}
	var err error
	if s.Start != "" {
		if out.start, err = parseClock(s.Start, false); err != nil {
			return nil, fmt.Errorf("start: %w", err)
		}
	}
	if s.End != "" {
		if out.end, err = parseClock(s.End, true); err != nil {
			return nil, fmt.Errorf("end: %w", err)
		}
	}
	if s.TZ != "" {
		if out.loc, err = time.LoadLocation(s.TZ); err != nil {
			return nil, fmt.Errorf("tz: %w", err)
		}
	}
	return out, nil
}

// parseClock parses "HH:MM"; "24:00" is accepted as an end time.
func parseClock(v string, end bool) (int, error) {
	hh, mm, ok := strings.Cut(strings.TrimSpace(v), ":")
	if !ok || len(hh) == 0 || len(hh) > 2 || len(mm) != 2 {
		return 0, fmt.Errorf("%q is not HH:MM", v)
	}
	h, herr := strconv.Atoi(hh)
	m, merr := strconv.Atoi(mm)
	switch {
	case herr != nil || merr != nil || h < 0 || m < 0 || m > 59:
		return 0, fmt.Errorf("%q is not HH:MM", v)
	case h == 24 && m == 0 && end:
		return minutesPerDay, nil
	case h > 23:
		return 0, fmt.Errorf("%q is not HH:MM", v)
	}
	return h*60 + m, nil
}

// active reports whether t falls inside the schedule.
func (s *schedule) active(t time.Time) bool {
	lt := t.In(s.loc)
	h, m, _ := lt.Clock()
	now := h*60 + m
	wd := lt.Weekday()
	if s.start < s.end {
		return s.days&(1<<wd) != 0 && now >= s.start && now < s.end
	}
	// The window crosses midnight (or lasts 24 hours when start == end):
	// the evening part belongs to today, the morning part to yesterday's
	// window.
	if now >= s.start && s.days&(1<<wd) != 0 {
		return true
	}
	yesterday := (wd + 6) % 7
	return now < s.end && s.days&(1<<yesterday) != 0
}
