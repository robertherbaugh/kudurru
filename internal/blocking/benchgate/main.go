// Command benchgate is the regression gate behind `make bench`
// (dev-docs/design-record.md §7.3, §12). It reads `go test -bench` output,
// takes the median of every benchmark metric over the -count runs, and
// fails when
//
//   - a metric is more than -threshold percent worse than the committed
//     baseline (and worse by more than a per-unit noise floor: 25 ns for
//     ns/op and medians, 150 ns for p99/p999, 64 B, 2 MB, 0.05 s),
//   - a metric breaks an absolute limit from the limits file (the §7.3
//     gates: suffix-set p99 < 2 µs, RSS < 150 MB for 3M entries, compile
//     < 5 s for 1M entries),
//   - a baseline metric is missing from the run, or the run contains FAIL
//     or panic lines.
//
// Usage:
//
//	benchgate [-baseline bench/baseline.txt] [-limits bench/limits.txt] [-threshold 20] current.txt
//
// Without -baseline only the limits are checked, which is how
// `make bench-baseline` validates a freshly recorded baseline.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"text/tabwriter"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("benchgate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	baselinePath := fs.String("baseline", "", "committed baseline (go test -bench output)")
	limitsPath := fs.String("limits", "", "limits file")
	threshold := fs.Float64("threshold", 20, "allowed regression in percent")
	strictCPU := fs.Bool("strict-cpu", false, "fail when the baseline was recorded on a different CPU")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		_, _ = fmt.Fprintln(stderr, "usage: benchgate [-baseline file] [-limits file] [-threshold pct] current.txt")
		return 2
	}

	current, err := parseFile(fs.Arg(0))
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	var baseline *Results
	if *baselinePath != "" {
		if baseline, err = parseFile(*baselinePath); err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return 2
		}
	}
	var rules []Rule
	if *limitsPath != "" {
		f, err := os.Open(*limitsPath)
		if err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return 2
		}
		rules, err = ParseRules(f)
		_ = f.Close()
		if err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return 2
		}
	}

	failed := false
	if len(current.Samples) == 0 {
		_, _ = fmt.Fprintln(stdout, "benchgate: no benchmark results in", fs.Arg(0))
		failed = true
	}
	for _, l := range current.Failures {
		_, _ = fmt.Fprintln(stdout, "benchgate: benchmark run failed:", l)
		failed = true
	}
	if baseline != nil && baseline.CPU != current.CPU {
		_, _ = fmt.Fprintf(stdout, "benchgate: WARNING baseline CPU %q differs from this run's %q; refresh the baseline on this runner class (make bench-baseline)\n", baseline.CPU, current.CPU)
		if *strictCPU {
			failed = true
		}
	}

	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "STATUS\tBENCHMARK\tUNIT\tBASELINE\tCURRENT\tDETAIL")
	for _, f := range Compare(baseline, current, rules, *threshold/100) {
		if f.Failed() {
			failed = true
		}
		b := "-"
		if f.HasBaseline {
			b = fmtNum(f.Baseline)
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", f.Status, f.Name, f.Unit, b, fmtNum(f.Current), f.Detail)
	}
	_ = tw.Flush()
	if failed {
		_, _ = fmt.Fprintln(stdout, "benchgate: FAIL")
		return 1
	}
	_, _ = fmt.Fprintln(stdout, "benchgate: ok")
	return 0
}

func parseFile(path string) (*Results, error) {
	f, err := os.Open(path) //nolint:gosec // paths come from the Makefile / the operator
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	r, err := Parse(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return r, nil
}
