package dnsdist

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"strings"
	"time"
)

var errConsole = errors.New("dnsdist: console command was not acknowledged")

const maxConsoleCommand = 32 << 10
const maxConsoleOutput = 64 << 10

// cappedOutput drains the child pipe even when its bounded capture is full.
type cappedOutput struct {
	data     []byte
	overflow bool
}

func (b *cappedOutput) Write(p []byte) (int, error) {
	n := len(p)
	remaining := maxConsoleOutput - len(b.data)
	if n > remaining {
		b.overflow = true
		p = p[:remaining]
	}
	b.data = append(b.data, p...)
	return n, nil
}

// console execs the upstream client; the key stays in the original private
// configuration. Neither arbitrary output, script nor argv enter an error.
func (s *Supervisor) console(ctx context.Context, statement string) error {
	if len(statement) > maxConsoleCommand-128 {
		return errConsole
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return errConsole
	}
	marker := "KUDURRU_ACK_" + hex.EncodeToString(nonce[:])
	script := statement + `; return "` + marker + `"`
	ctx, cancel := context.WithTimeout(ctx, s.cfg.ConsoleTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.cfg.Binary, "-C", s.MainPath(), "-e", script) //nolint:gosec // operator binary; fixed client arguments, no shell or key argument
	cmd.Env = append(os.Environ(), s.cfg.ChildEnv...)
	cmd.WaitDelay = 250 * time.Millisecond
	var out, stderr cappedOutput
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil || out.overflow || stderr.overflow || !finalAcknowledgment(out.data, marker) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errConsole
	}
	return nil
}

// check bounds and sanitizes upstream diagnostics too: malformed certificates
// and Lua diagnostics must not disclose source material or configuration keys.
func (s *Supervisor) check(ctx context.Context, path string) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.cfg.Binary, "--check-config", "-C", path) //nolint:gosec // operator executable, fixed arguments
	cmd.Env = append(os.Environ(), s.cfg.ChildEnv...)
	cmd.WaitDelay = 250 * time.Millisecond
	var output cappedOutput
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Run(); err != nil || output.overflow {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("dnsdist: configuration check failed")
	}
	return nil
}

// Lua configuration clients can emit local configuration-only diagnostics
// before the server's response. Only one exact final line is acknowledgment;
// a marker embedded in a diagnostic/echo or followed by an error is not.
func finalAcknowledgment(output []byte, marker string) bool {
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if strings.TrimSpace(lines[len(lines)-1]) != marker {
		return false
	}
	count := 0
	for _, line := range lines {
		if strings.TrimSpace(line) == marker {
			count++
		}
	}
	return count == 1
}
