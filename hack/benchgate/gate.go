package main

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// Results holds parsed `go test -bench` output: per benchmark (name without
// the -GOMAXPROCS suffix) and unit, the samples of every -count run.
type Results struct {
	CPU     string
	Samples map[string]map[string][]float64
	// Failures are FAIL / panic lines; a run with failures never passes.
	Failures []string
	order    []string
}

var procSuffix = regexp.MustCompile(`-[0-9]+$`)

// Parse reads benchmark output. Lines that are not benchmark results, cpu
// headers or failures are ignored, so comments may be added freely.
func Parse(r io.Reader) (*Results, error) {
	res := &Results{Samples: map[string]map[string][]float64{}}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(line, "cpu:"):
			res.CPU = strings.TrimSpace(strings.TrimPrefix(line, "cpu:"))
		case strings.HasPrefix(line, "FAIL"), strings.HasPrefix(line, "--- FAIL"), strings.HasPrefix(line, "panic:"):
			res.Failures = append(res.Failures, line)
		case strings.HasPrefix(line, "Benchmark"):
			f := strings.Fields(line)
			if len(f) < 4 || len(f)%2 != 0 {
				continue // a benchmark name echoed without results
			}
			if _, err := strconv.ParseInt(f[1], 10, 64); err != nil {
				continue
			}
			name := procSuffix.ReplaceAllString(f[0], "")
			units := res.Samples[name]
			if units == nil {
				units = map[string][]float64{}
				res.Samples[name] = units
				res.order = append(res.order, name)
			}
			for i := 2; i+1 < len(f); i += 2 {
				v, err := strconv.ParseFloat(f[i], 64)
				if err != nil {
					return nil, fmt.Errorf("benchgate: bad value %q in %q", f[i], line)
				}
				units[f[i+1]] = append(units[f[i+1]], v)
			}
		}
	}
	return res, sc.Err()
}

// Median returns the median sample of a benchmark unit and whether it exists.
func (r *Results) Median(name, unit string) (float64, bool) {
	s := r.Samples[name][unit]
	if len(s) == 0 {
		return 0, false
	}
	s = slices.Clone(s)
	sort.Float64s(s)
	if len(s)%2 == 1 {
		return s[len(s)/2], true
	}
	return (s[len(s)/2-1] + s[len(s)/2]) / 2, true
}

// Rule is a line of the limits file.
type Rule struct {
	Ignore bool
	Name   *regexp.Regexp
	Unit   string
	Max    float64
}

// ParseRules reads the limits file:
//
//	limit  <benchmark regexp> <unit> <max>
//	ignore <benchmark regexp> <unit>
//
// Blank lines and '#' comments are skipped.
func ParseRules(r io.Reader) ([]Rule, error) {
	var rules []Rule
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		var rule Rule
		switch {
		case f[0] == "limit" && len(f) == 4:
			v, err := strconv.ParseFloat(f[3], 64)
			if err != nil {
				return nil, fmt.Errorf("benchgate: limits line %d: %w", n, err)
			}
			rule.Max = v
		case f[0] == "ignore" && len(f) == 3:
			rule.Ignore = true
		default:
			return nil, fmt.Errorf("benchgate: limits line %d: want \"limit <re> <unit> <max>\" or \"ignore <re> <unit>\"", n)
		}
		re, err := regexp.Compile(f[1])
		if err != nil {
			return nil, fmt.Errorf("benchgate: limits line %d: %w", n, err)
		}
		rule.Name, rule.Unit = re, f[2]
		rules = append(rules, rule)
	}
	return rules, sc.Err()
}

// higherIsBetter lists units where a drop is the regression.
var higherIsBetter = map[string]bool{"MB/s": true}

// floor returns the absolute change below which a relative regression is
// treated as noise, per unit.
func floor(unit string) float64 {
	switch {
	case unit == "p99-ns" || unit == "p999-ns":
		// Tail percentiles move by ~100 ns with interrupts and preemption
		// on shared runners; a smaller change says nothing at the 2 µs
		// scale of the gate.
		return 150
	case unit == "ns/op" || strings.HasSuffix(unit, "-ns"):
		return 25
	case unit == "B/op":
		return 64
	case strings.HasSuffix(unit, "-MB"):
		return 2
	case strings.HasSuffix(unit, "-s"):
		return 0.05
	}
	return 0
}

// Finding is one compared value.
type Finding struct {
	Name, Unit        string
	Baseline, Current float64
	HasBaseline       bool
	Status            string // ok, regression, improved, limit, missing, new
	Detail            string
}

// Failed reports whether the finding fails the gate.
func (f Finding) Failed() bool {
	return f.Status == "regression" || f.Status == "limit" || f.Status == "missing"
}

// Compare checks current against baseline (which may be nil to check only
// the limits) with a relative threshold such as 0.20.
func Compare(baseline, current *Results, rules []Rule, threshold float64) []Finding {
	ignored := func(name, unit string) bool {
		for _, r := range rules {
			if r.Ignore && r.Unit == unit && r.Name.MatchString(name) {
				return true
			}
		}
		return false
	}
	var out []Finding
	if baseline != nil {
		for _, name := range baseline.order {
			for _, unit := range sortedUnits(baseline.Samples[name]) {
				if ignored(name, unit) {
					continue
				}
				b, _ := baseline.Median(name, unit)
				c, ok := current.Median(name, unit)
				f := Finding{Name: name, Unit: unit, Baseline: b, Current: c, HasBaseline: true}
				switch {
				case !ok:
					f.Status, f.Detail = "missing", "in the baseline but not in this run"
				default:
					f.Status, f.Detail = judge(unit, b, c, threshold)
				}
				out = append(out, f)
			}
		}
	}
	for _, name := range current.order {
		for _, unit := range sortedUnits(current.Samples[name]) {
			c, _ := current.Median(name, unit)
			for _, r := range rules {
				if !r.Ignore && r.Unit == unit && r.Name.MatchString(name) && c > r.Max {
					out = append(out, Finding{Name: name, Unit: unit, Current: c, Status: "limit",
						Detail: fmt.Sprintf("median %s exceeds the limit %s", fmtNum(c), fmtNum(r.Max))})
				}
			}
			if baseline != nil && !ignored(name, unit) {
				if _, ok := baseline.Median(name, unit); !ok {
					out = append(out, Finding{Name: name, Unit: unit, Current: c, Status: "new", Detail: "no baseline yet"})
				}
			}
		}
	}
	return out
}

func judge(unit string, b, c, threshold float64) (string, string) {
	delta := c - b
	if higherIsBetter[unit] {
		delta = -delta
	}
	rel := math.Inf(1)
	if b != 0 {
		rel = delta / math.Abs(b)
	}
	detail := fmt.Sprintf("%+.1f%%", 100*relChange(b, c))
	switch {
	case b == 0 && c == 0:
		return "ok", "+0.0%"
	case delta > floor(unit) && rel > threshold:
		return "regression", detail
	case -delta > floor(unit) && -rel > threshold:
		return "improved", detail + " (consider refreshing the baseline)"
	}
	return "ok", detail
}

// relChange is the signed relative change from b to c.
func relChange(b, c float64) float64 {
	if b == 0 {
		if c == 0 {
			return 0
		}
		return math.Inf(1)
	}
	return (c - b) / math.Abs(b)
}

func sortedUnits(m map[string][]float64) []string {
	units := make([]string, 0, len(m))
	for u := range m {
		units = append(units, u)
	}
	sort.Strings(units)
	return units
}

func fmtNum(v float64) string {
	switch {
	case v == math.Trunc(v) && math.Abs(v) < 1e15:
		return strconv.FormatFloat(v, 'f', 0, 64)
	case math.Abs(v) >= 100:
		return strconv.FormatFloat(v, 'f', 1, 64)
	}
	return strconv.FormatFloat(v, 'g', 4, 64)
}
