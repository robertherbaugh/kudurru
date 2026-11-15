package main

import (
	"testing"
	"time"
)

// apply and restore send every document in one request that the server lets
// run for five minutes (D378), so their default --request-timeout is the same
// five minutes. An explicit --request-timeout still wins, and other commands
// keep the global default.
func TestApplyAndRestoreDefaultToTheRequestLifetime(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want time.Duration
	}{
		{[]string{"apply", "-f", "objects.yaml"}, 5 * time.Minute},
		{[]string{"restore", "-f", "backup.yaml"}, 5 * time.Minute},
		{[]string{"apply", "-f", "objects.yaml", "--request-timeout", "10s"}, 10 * time.Second},
		{[]string{"restore", "--request-timeout", "45s", "-f", "backup.yaml"}, 45 * time.Second},
	} {
		o := &cliOpts{}
		root := newRootWith(o)
		cmd, rest, err := root.Find(tc.args)
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.ParseFlags(rest); err != nil {
			t.Fatal(err)
		}
		if o.timeout != 30*time.Second && o.timeout != tc.want {
			t.Fatalf("%v: unexpected parsed timeout %s", tc.args, o.timeout)
		}
		o.multiItemTimeout(cmd)
		if o.timeout != tc.want {
			t.Fatalf("%v: request timeout %s, want %s", tc.args, o.timeout, tc.want)
		}
	}
	o := &cliOpts{}
	root := newRootWith(o)
	cmd, rest, err := root.Find([]string{"get", "dnszones"})
	if err != nil || cmd.ParseFlags(rest) != nil || o.timeout != 30*time.Second {
		t.Fatal("other commands lost the global default", err, o.timeout)
	}
}
