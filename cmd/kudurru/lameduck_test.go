package main

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/health"
)

func TestLameduckCmd(t *testing.T) {
	checker := health.New(nil)
	srv := httptest.NewServer(checker.Handler())
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")

	run := func(args ...string) (string, error) {
		cmd := newLameduckCmd()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(append([]string{"--ops-address", addr}, args...))
		err := cmd.Execute()
		return out.String(), err
	}
	start := time.Now()
	out, err := run("200ms")
	if err != nil || !checker.IsLameduck() || !strings.Contains(out, "draining for 200ms") {
		t.Fatalf("lameduck: %v %q lameduck=%v", err, out, checker.IsLameduck())
	}
	if time.Since(start) < 200*time.Millisecond {
		t.Fatal("the command must wait for the duration")
	}
	if ok, _ := checker.Ready(); ok {
		t.Fatal("ready during lameduck")
	}
	if _, err := run("--cancel"); err != nil || checker.IsLameduck() {
		t.Fatalf("cancel: %v lameduck=%v", err, checker.IsLameduck())
	}
	if _, err := run("soon"); err == nil {
		t.Fatal("a bad duration must fail")
	}
}
