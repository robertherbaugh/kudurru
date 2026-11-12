package main

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// processSchedStat sums, over every thread of this process, the time spent
// running and the time spent runnable but waiting for a CPU (the first two
// fields of /proc/self/task/*/schedstat, in nanoseconds). ok is false where
// the kernel does not expose them.
func processSchedStat() (run, wait time.Duration, ok bool) {
	entries, err := os.ReadDir("/proc/self/task")
	if err != nil {
		return 0, 0, false
	}
	for _, entry := range entries {
		data, err := os.ReadFile("/proc/self/task/" + entry.Name() + "/schedstat")
		if err != nil {
			continue // the thread exited
		}
		fields := strings.Fields(string(data))
		if len(fields) < 2 {
			return 0, 0, false
		}
		r, errRun := strconv.ParseInt(fields[0], 10, 64)
		w, errWait := strconv.ParseInt(fields[1], 10, 64)
		if errRun != nil || errWait != nil {
			return 0, 0, false
		}
		run, wait, ok = run+time.Duration(r), wait+time.Duration(w), true
	}
	return run, wait, ok
}

// readinessBudget bounds a mounted server's startup wait in time the process
// could actually run. A node's startup is CPU bound: under -race a static node
// needs about 10 s of one CPU to become ready, so on a shared machine the
// same work can take several times longer in wall time while its threads wait
// for a CPU. The budget charges wall time less the starved part of it: the
// run-queue wait the process's threads accumulated in the same period, divided
// by the number of Ps (GOMAXPROCS), which is how many of them can run at
// once. A real stall (no progress, or progress that never converges, on a
// machine with CPUs to spare) accumulates no such wait and still fails after
// budget, while time the scheduler gave to other processes does not count.
// ceiling bounds the wall time however starved the process is. Where the
// kernel exposes no scheduler statistics, budget is plain wall time.
type readinessBudget struct {
	start           time.Time
	budget, ceiling time.Duration
	procs           int
	run, wait       time.Duration
	ok              bool
	stat            func() (run, wait time.Duration, ok bool)
}

func newReadinessBudget(budget, ceiling time.Duration) *readinessBudget {
	return newReadinessBudgetWith(budget, ceiling, runtime.GOMAXPROCS(0), processSchedStat, time.Now())
}

func newReadinessBudgetWith(budget, ceiling time.Duration, procs int, stat func() (time.Duration, time.Duration, bool), start time.Time) *readinessBudget {
	b := &readinessBudget{start: start, budget: budget, ceiling: ceiling, procs: max(procs, 1), stat: stat}
	b.run, b.wait, b.ok = stat()
	return b
}

// since is the process's CPU time and summed run-queue wait since start.
func (b *readinessBudget) since() (run, wait time.Duration, ok bool) {
	if !b.ok {
		return 0, 0, false
	}
	r, w, ok := b.stat()
	if !ok {
		return 0, 0, false
	}
	return r - b.run, w - b.wait, true
}

// expired reports whether the charged time exceeds budget or the wall time
// exceeds ceiling. It reads scheduler statistics only after budget wall time.
func (b *readinessBudget) expired(now time.Time) bool {
	wall := now.Sub(b.start)
	if wall <= b.budget {
		return false
	}
	if wall > b.ceiling {
		return true
	}
	_, wait, ok := b.since()
	return !ok || wall-wait/time.Duration(b.procs) > b.budget
}

// String names the wall time, the process's CPU time and its summed run-queue
// wait since start, for a log line or a failure message.
func (b *readinessBudget) String() string {
	run, wait, ok := b.since()
	wall := time.Since(b.start).Round(time.Millisecond)
	if !ok {
		return fmt.Sprintf("wall=%s (no scheduler statistics)", wall)
	}
	return fmt.Sprintf("wall=%s process-cpu=%s runqueue-wait=%s gomaxprocs=%d", wall, run.Round(time.Millisecond), wait.Round(time.Millisecond), b.procs)
}

func TestReadinessBudgetChargesOnlyRunnableTime(t *testing.T) {
	start := time.Unix(1_000_000, 0)
	var wait time.Duration
	stat := func() (time.Duration, time.Duration, bool) { return time.Second, wait, true }
	b := newReadinessBudgetWith(20*time.Second, 2*time.Minute, 1, stat, start)
	if b.expired(start.Add(20 * time.Second)) {
		t.Fatal("budget expired at its own length")
	}
	if !b.expired(start.Add(21 * time.Second)) {
		t.Fatal("an unstarved process kept waiting past budget")
	}
	wait = 30 * time.Second
	if b.expired(start.Add(45 * time.Second)) {
		t.Fatal("run-queue wait was charged to the server")
	}
	if !b.expired(start.Add(51 * time.Second)) {
		t.Fatal("budget did not expire after the runnable time it allows")
	}
	wait = time.Hour
	if b.expired(start.Add(2*time.Minute)) || !b.expired(start.Add(2*time.Minute+time.Nanosecond)) {
		t.Fatal("ceiling not enforced")
	}

	// Four Ps: 40 s of summed wait is 10 s during which every P waited.
	wait = 0
	four := newReadinessBudgetWith(20*time.Second, 2*time.Minute, 4, stat, start)
	wait = 40 * time.Second
	if four.expired(start.Add(30*time.Second)) || !four.expired(start.Add(31*time.Second)) {
		t.Fatal("run-queue wait not shared across Ps")
	}

	none := newReadinessBudgetWith(20*time.Second, 2*time.Minute, 1, func() (time.Duration, time.Duration, bool) { return 0, 0, false }, start)
	if none.expired(start.Add(20*time.Second)) || !none.expired(start.Add(21*time.Second)) {
		t.Fatal("without scheduler statistics the budget is not plain wall time")
	}
	if got := none.String(); !strings.Contains(got, "no scheduler statistics") {
		t.Fatal("missing statistics not reported", got)
	}
}

func TestProcessSchedStatReadsThisProcess(t *testing.T) {
	run, wait, ok := processSchedStat()
	if _, err := os.Stat("/proc/self/task"); err != nil {
		if ok {
			t.Fatal("statistics reported without /proc")
		}
		return
	}
	if !ok || run <= 0 || wait < 0 {
		t.Fatal("this process's scheduler statistics unavailable", run, wait, ok)
	}
	deadline := time.Now().Add(50 * time.Millisecond)
	spins := 0
	for time.Now().Before(deadline) {
		spins++ // run on a CPU, so run time must advance
	}
	later, laterWait, ok := processSchedStat()
	if !ok || later <= run || laterWait < wait {
		t.Fatal("scheduler statistics did not advance", spins, run, later, wait, laterWait)
	}
}
