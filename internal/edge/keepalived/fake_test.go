package keepalived

// A fake keepalived and a fake `kudurru vip fifo` for the supervisor
// tests. The test binary re-executes itself through symlinks named
// "keepalived" and "kudurru". The fake keepalived checks the rendered
// file structurally (-t), and in run mode creates the notify FIFO, starts
// the configured FIFO script with the FIFO path appended (as keepalived
// does) and announces BACKUP for every instance, then MASTER for the
// first. It logs to <config dir>/fake.log.

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func errorf(format string, args ...any) error { return fmt.Errorf(format, args...) }

func TestMain(m *testing.M) {
	switch filepath.Base(os.Args[0]) {
	case "keepalived":
		os.Exit(fakeKeepalived(os.Args[1:]))
	case "kudurru":
		os.Exit(fakeFIFOScript(os.Args[1:]))
	}
	os.Exit(m.Run())
}

func fakeLog(dir, format string, args ...any) {
	f, err := os.OpenFile(filepath.Join(dir, "fake.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(f, format+"\n", args...)
	_ = f.Close()
}

type fakeConf struct {
	fifo, script string
	instances    []string
	priorities   []string
}

func parseFakeConf(path string) (*fakeConf, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if strings.Contains(string(b), "FAIL-CHECK") {
		return nil, fmt.Errorf("fake keepalived: refusing %s", path)
	}
	if err := checkSyntax(b); err != nil {
		return nil, err
	}
	fc := &fakeConf{}
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Fields(l)
		if len(f) < 2 {
			continue
		}
		switch f[0] {
		case "vrrp_notify_fifo":
			fc.fifo = f[1]
		case "vrrp_notify_fifo_script":
			fc.script = strings.Trim(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), "vrrp_notify_fifo_script")), `"`)
		case "vrrp_instance":
			fc.instances = append(fc.instances, f[1])
		case "priority":
			fc.priorities = append(fc.priorities, f[1])
		}
	}
	if fc.fifo == "" || fc.script == "" || len(fc.instances) == 0 || !strings.Contains(string(b), "global_defs") {
		return nil, fmt.Errorf("fake keepalived: %s lacks global_defs, the FIFO or instances", path)
	}
	return fc, nil
}

func fakeKeepalived(args []string) int {
	var path string
	test := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-t":
			test = true
		case "-f", "--use-file":
			i++
			if i < len(args) {
				path = args[i]
			}
		}
	}
	fc, err := parseFakeConf(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	dir := filepath.Dir(path)
	if test {
		fakeLog(dir, "check %s", filepath.Base(path))
		return 0
	}
	_ = os.Remove(fc.fifo)
	if err := syscall.Mkfifo(fc.fifo, 0o600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	argv := append(strings.Fields(fc.script), fc.fifo)
	script := exec.Command(argv[0], argv[1:]...) //nolint:gosec // test fake
	script.Stdout, script.Stderr = os.Stderr, os.Stderr
	if err := script.Start(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fifo, err := os.OpenFile(fc.fifo, os.O_WRONLY, 0)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fakeLog(dir, "start pid=%d instances=%s", os.Getpid(), strings.Join(fc.instances, ","))
	for i, inst := range fc.instances {
		_, _ = fmt.Fprintf(fifo, "INSTANCE %q BACKUP %s\n", inst, fc.priorities[i])
	}
	time.Sleep(50 * time.Millisecond)
	_, _ = fmt.Fprintf(fifo, "INSTANCE %q MASTER %s\n", fc.instances[0], fc.priorities[0])
	_, _ = fmt.Fprintf(fifo, "garbage line\n")
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGHUP)
	for s := range sig {
		if s == syscall.SIGHUP {
			if _, err := parseFakeConf(path); err != nil {
				fakeLog(dir, "hup-invalid")
				continue
			}
			fakeLog(dir, "hup")
			continue
		}
		break
	}
	for i, inst := range fc.instances {
		_, _ = fmt.Fprintf(fifo, "INSTANCE %q STOP %s\n", inst, fc.priorities[i])
	}
	_ = fifo.Close()
	time.Sleep(50 * time.Millisecond)
	_ = script.Process.Signal(syscall.SIGTERM)
	_ = script.Wait()
	fakeLog(dir, "stop pid=%d", os.Getpid())
	return 0
}

// fakeFIFOScript is `kudurru vip fifo --node N [--run-dir D] <fifo>`.
func fakeFIFOScript(args []string) int {
	if len(args) < 2 || args[0] != "vip" || args[1] != "fifo" {
		fmt.Fprintln(os.Stderr, "fake kudurru: want vip fifo")
		return 2
	}
	c := &Consumer{RunDir: DefaultRunDir, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	var fifo string
	for i := 2; i < len(args); i++ {
		switch args[i] {
		case "--node":
			i++
			c.Node = args[i]
		case "--run-dir":
			i++
			c.RunDir = args[i]
		default:
			fifo = args[i]
		}
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer cancel()
	if err := c.ConsumeFIFO(ctx, fifo); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}
