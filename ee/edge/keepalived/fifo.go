package keepalived

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/robertherbaugh/kudurru/ee/edge"
)

// VRRP states as keepalived reports them on the notify FIFO.
const (
	StateMaster = "MASTER"
	StateBackup = "BACKUP"
	StateFault  = "FAULT"
	StateStop   = "STOP"
)

// Notification is one notify FIFO line: `INSTANCE "VI_dns" MASTER 200`.
type Notification struct {
	// Kind is INSTANCE or GROUP.
	Kind string
	// Name is the instance or group name (VI_<vip> for kudurru instances).
	Name string
	// State is MASTER, BACKUP, FAULT, STOP, DELETED or a priority event
	// (MASTER_PRIORITY, BACKUP_PRIORITY, MASTER_RX_LOWER_PRI).
	State    string
	Priority int
}

// VIP returns the kudurru VIP name of an INSTANCE notification.
func (n Notification) VIP() (string, bool) {
	if n.Kind != "INSTANCE" || !strings.HasPrefix(n.Name, "VI_") {
		return "", false
	}
	return strings.TrimPrefix(n.Name, "VI_"), true
}

// ParseLine parses one FIFO line.
func ParseLine(line string) (Notification, error) {
	line = strings.TrimSpace(line)
	kind, rest, ok := strings.Cut(line, " ")
	if !ok || (kind != "INSTANCE" && kind != "GROUP") {
		return Notification{}, fmt.Errorf("keepalived: unexpected FIFO line %q", line)
	}
	rest = strings.TrimSpace(rest)
	if !strings.HasPrefix(rest, `"`) {
		return Notification{}, fmt.Errorf("keepalived: unquoted name in FIFO line %q", line)
	}
	end := strings.IndexByte(rest[1:], '"')
	if end < 0 {
		return Notification{}, fmt.Errorf("keepalived: unterminated name in FIFO line %q", line)
	}
	n := Notification{Kind: kind, Name: rest[1 : end+1]}
	if strings.ContainsFunc(n.Name, unicode.IsControl) {
		return Notification{}, fmt.Errorf("keepalived: control character in name in FIFO line %q", line)
	}
	fields := strings.Fields(rest[end+2:])
	if len(fields) == 0 {
		return Notification{}, fmt.Errorf("keepalived: no state in FIFO line %q", line)
	}
	n.State = fields[0]
	if len(fields) > 1 {
		p, err := strconv.Atoi(fields[1])
		if err != nil {
			return Notification{}, fmt.Errorf("keepalived: priority in FIFO line %q: %w", line, err)
		}
		n.Priority = p
	}
	return n, nil
}

// VIPState is the last known state of one VIP on this node.
type VIPState struct {
	State    string    `json:"state"`
	Priority int       `json:"priority"`
	Since    time.Time `json:"since"`
}

// StateFile is what `kudurru vip fifo` maintains in <run dir>/vip-state.json
// for the supervisor (metrics, logs), the keepalived vip.Provider in the
// local kudurru (Owner, the Nodes page) and `kudurru vip rebalance`.
type StateFile struct {
	Node    string              `json:"node"`
	Updated time.Time           `json:"updated"`
	VIPs    map[string]VIPState `json:"vips"`
	// Session and Sequence identify one Consumer episode. Transitions is a
	// bounded journal, not a heartbeat or proof of current physical ownership.
	// Older state-file readers safely ignore these additive fields.
	Session     string       `json:"session,omitempty"`
	Sequence    uint64       `json:"sequence,omitempty"`
	Transitions []Transition `json:"transitions,omitempty"`
}

// Transition retains notifications between bounded observation polls.
type Transition struct {
	Sequence uint64   `json:"sequence"`
	VIP      string   `json:"vip"`
	Previous VIPState `json:"previous"`
	Next     VIPState `json:"next"`
}

// TransitionLimit bounds the on-disk notification journal.
const TransitionLimit = 64

// StatePath is the state file in runDir.
func StatePath(runDir string) string { return filepath.Join(runDir, "vip-state.json") }

// ReadState reads the state file; a missing file is an empty state.
func ReadState(runDir string) (StateFile, error) {
	b, err := os.ReadFile(StatePath(runDir))
	if errors.Is(err, os.ErrNotExist) {
		return StateFile{VIPs: map[string]VIPState{}}, nil
	}
	if err != nil {
		return StateFile{}, err
	}
	var s StateFile
	if err := json.Unmarshal(b, &s); err != nil {
		return StateFile{}, fmt.Errorf("keepalived: %s: %w", StatePath(runDir), err)
	}
	if s.VIPs == nil {
		s.VIPs = map[string]VIPState{}
	}
	return s, nil
}

// Consumer reads notify FIFO lines and maintains the state file. It never
// blocks keepalived for longer than one small atomic file write per line.
type Consumer struct {
	RunDir string
	Node   string
	// Now defaults to time.Now.
	Now func() time.Time
	// Log defaults to slog.Default().
	Log *slog.Logger
	// OnChange is called synchronously for every VIP state change; it
	// must not block.
	OnChange func(vip string, prev, next VIPState)

	mu    sync.Mutex
	state StateFile
}

func (c *Consumer) init() {
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Log == nil {
		c.Log = slog.Default()
	}
	if c.state.VIPs == nil {
		// keepalived (re)started us: whatever the file said is stale.
		c.state = StateFile{Node: c.Node, Updated: c.Now().UTC(), VIPs: map[string]VIPState{}, Session: rand.Text()}
	}
}

// Reset writes an empty state (keepalived started; no VIP is held yet).
func (c *Consumer) Reset() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state = StateFile{}
	c.init()
	return c.writeLocked()
}

func (c *Consumer) writeLocked() error {
	b, err := json.MarshalIndent(c.state, "", "  ")
	if err != nil {
		return err
	}
	return edge.WriteFileAtomic(StatePath(c.RunDir), append(b, '\n'), 0o600)
}

// Handle applies one FIFO line.
func (c *Consumer) Handle(line string) error {
	n, err := ParseLine(line)
	if err != nil {
		return err
	}
	vipName, ok := n.VIP()
	if !ok {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.init()
	prev, had := c.state.VIPs[vipName]
	next := prev
	switch n.State {
	case "MASTER_PRIORITY":
		next.State, next.Priority = StateMaster, n.Priority
	case "BACKUP_PRIORITY":
		next.State, next.Priority = StateBackup, n.Priority
	case "MASTER_RX_LOWER_PRI":
		return nil // informational: a lower-priority advert while MASTER
	case "DELETED":
		next.State = StateStop
	case StateMaster, StateBackup, StateFault, StateStop:
		next.State, next.Priority = n.State, n.Priority
	default:
		c.Log.Debug("ignoring FIFO state", "line", line)
		return nil
	}
	if had && next.State == prev.State && next.Priority == prev.Priority {
		return nil
	}
	now := c.Now().UTC()
	if !had || next.State != prev.State {
		next.Since = now
	}
	c.state.VIPs[vipName] = next
	c.state.Updated = now
	if !had || next.State != prev.State {
		c.state.Sequence++
		c.state.Transitions = append(c.state.Transitions, Transition{Sequence: c.state.Sequence, VIP: vipName, Previous: prev, Next: next})
		if len(c.state.Transitions) > TransitionLimit {
			c.state.Transitions = c.state.Transitions[len(c.state.Transitions)-TransitionLimit:]
		}
	}
	if err := c.writeLocked(); err != nil {
		return err
	}
	c.Log.Info("vrrp state", "event", "vip.master_changed", "vip", vipName, "node", c.Node, "state", next.State, "priority", next.Priority, "previous", prev.State)
	if c.OnChange != nil {
		c.OnChange(vipName, prev, next)
	}
	return nil
}

// Run consumes lines from r until EOF or ctx ends. Bad lines are logged
// and skipped.
func (c *Consumer) Run(ctx context.Context, r io.Reader) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 4096), 64<<10)
	for sc.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := c.Handle(sc.Text()); err != nil {
			c.Log.Warn("notify FIFO", "err", err)
		}
	}
	return sc.Err()
}

// ConsumeFIFO opens the FIFO at path and consumes it until ctx ends. The
// FIFO is opened read-write (Linux): the open never blocks waiting for
// keepalived, and the reader never sees EOF when keepalived closes and
// reopens its end on reload. Read errors re-open the FIFO.
func (c *Consumer) ConsumeFIFO(ctx context.Context, path string) error {
	if err := c.Reset(); err != nil {
		return err
	}
	for ctx.Err() == nil {
		f, err := os.OpenFile(path, os.O_RDWR, 0) //nolint:gosec // keepalived passes the FIFO path
		if err != nil {
			c.Log.Warn("open notify FIFO", "path", path, "err", err)
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
			}
			continue
		}
		stop := make(chan struct{})
		go func() {
			select {
			case <-ctx.Done():
				_ = f.Close() // unblocks the read
			case <-stop:
			}
		}()
		err = c.Run(ctx, f)
		close(stop)
		_ = f.Close()
		if err != nil && ctx.Err() == nil && !errors.Is(err, os.ErrClosed) {
			c.Log.Warn("notify FIFO read", "err", err)
		}
		if ctx.Err() == nil {
			select {
			case <-ctx.Done():
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
	return nil
}
