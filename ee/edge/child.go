package edge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Child is one supervised program (dnsdist or keepalived) started by a
// kudurru supervisor that is PID 1 of the container (D16). It is not
// restarted by itself; the supervisor decides.
type Child struct {
	// Path is the binary; Args its arguments (without argv[0]).
	Path string
	Args []string
	// Env is appended to the supervisor's environment.
	Env []string
	// Stdout and Stderr default to the supervisor's.
	Stdout, Stderr io.Writer
	// StopTimeout bounds the SIGTERM grace period before SIGKILL;
	// default 10 s.
	StopTimeout time.Duration

	mu   sync.Mutex
	cmd  *exec.Cmd
	done chan struct{}
	err  error
}

// Start launches the program.
func (c *Child) Start() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cmd != nil {
		return errors.New("edge: child already started")
	}
	cmd := exec.Command(c.Path, c.Args...) //nolint:gosec // the binary path is the operator's --dnsdist-bin/--keepalived-bin
	cmd.Stdout, cmd.Stderr = c.Stdout, c.Stderr
	if cmd.Stdout == nil {
		cmd.Stdout = os.Stdout
	}
	if cmd.Stderr == nil {
		cmd.Stderr = os.Stderr
	}
	if len(c.Env) > 0 {
		cmd.Env = append(os.Environ(), c.Env...)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("edge: start %s: %w", c.Path, err)
	}
	c.cmd = cmd
	c.done = make(chan struct{})
	go func() {
		err := cmd.Wait()
		c.mu.Lock()
		c.err = err
		c.mu.Unlock()
		close(c.done)
	}()
	return nil
}

// Done is closed when the program has exited; nil before Start.
func (c *Child) Done() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.done
}

// Err is the exit error once Done is closed.
func (c *Child) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// Pid is the process id, 0 before Start.
func (c *Child) Pid() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cmd == nil || c.cmd.Process == nil {
		return 0
	}
	return c.cmd.Process.Pid
}

// Running reports whether the program was started and has not exited.
func (c *Child) Running() bool {
	d := c.Done()
	if d == nil {
		return false
	}
	select {
	case <-d:
		return false
	default:
		return true
	}
}

// Signal forwards sig to the program; a no-op once it exited.
func (c *Child) Signal(sig os.Signal) error {
	if !c.Running() {
		return nil
	}
	c.mu.Lock()
	p := c.cmd.Process
	c.mu.Unlock()
	if err := p.Signal(sig); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}

// Stop sends SIGTERM, waits up to StopTimeout and then kills the program.
// It returns once the program has exited.
func (c *Child) Stop() {
	d := c.Done()
	if d == nil {
		return
	}
	timeout := c.StopTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	_ = c.Signal(syscall.SIGTERM)
	select {
	case <-d:
		return
	case <-time.After(timeout):
	}
	_ = c.Signal(syscall.SIGKILL)
	<-d
}

// Check runs a validator (`dnsdist --check-config`, `keepalived -t`) and
// returns its combined output; a non-zero exit is an error carrying the
// output.
func Check(ctx context.Context, path string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, path, args...) //nolint:gosec // operator-supplied validator binary
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	s := strings.TrimSpace(out.String())
	if err != nil {
		return s, fmt.Errorf("edge: %s %s: %w: %s", filepath.Base(path), strings.Join(args, " "), err, s)
	}
	return s, nil
}

// WriteFileAtomic writes data to a temporary file next to path, syncs it
// and renames it over path, so readers see the old or the new content and
// never a partial file.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Chmod(perm); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// SwapFile installs candidate as path: the current path (if any) is
// copied to path+".prev" first, then candidate is renamed over path.
func SwapFile(candidate, path string) error {
	if old, err := os.ReadFile(path); err == nil { //nolint:gosec // supervisor-owned config path
		if err := WriteFileAtomic(path+".prev", old, 0o600); err != nil {
			return fmt.Errorf("edge: keep previous %s: %w", path, err)
		}
	}
	return os.Rename(candidate, path)
}
