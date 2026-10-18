package dhcpha

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"time"

	"github.com/hashicorp/raft"
	"github.com/robertherbaugh/kudurru/internal/dhcp"
)

type command struct {
	Scope       string              `json:"scope,omitempty"`
	Lease       dhcp.ReplicaCommand `json:"lease"`
	Seed        map[string][]byte   `json:"seed,omitempty"`
	Fingerprint string              `json:"fingerprint"`
}

type applyResult struct {
	Result dhcp.ReplicaResult
	Err    error
}
type failure struct{ err error }
type machine struct {
	stores       map[string]*dhcp.LeaseStore
	names        []string
	fingerprint  string
	initialized  atomic.Bool
	failed       atomic.Pointer[failure]
	lastTime     atomic.Int64
	appliedIndex atomic.Uint64
	markStarted  func() error
	stop         func(error)
}

func (m *machine) fail(err error) applyResult {
	if m.failed.CompareAndSwap(nil, &failure{err}) && m.stop != nil {
		m.stop(err)
	}
	return applyResult{Err: errors.Join(ErrUnavailable, err)}
}

func (m *machine) Apply(log *raft.Log) interface{} {
	if f := m.failed.Load(); f != nil {
		return applyResult{Err: errors.Join(ErrUnavailable, f.err)}
	}
	var cmd command
	if err := json.Unmarshal(log.Data, &cmd); err != nil {
		return m.fail(fmt.Errorf("decode committed command: %w", err))
	}
	if cmd.Fingerprint != m.fingerprint {
		return m.fail(errors.New("committed scope topology differs from local topology"))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if cmd.Seed != nil {
		if m.initialized.Load() {
			return applyResult{}
		}
		if len(cmd.Seed) != len(m.stores) {
			return m.fail(errors.New("bootstrap seed has a different scope registry"))
		}
		if m.markStarted != nil {
			if err := m.markStarted(); err != nil {
				return m.fail(err)
			}
		}
		for _, name := range m.names {
			state, ok := cmd.Seed[name]
			if !ok {
				return m.fail(fmt.Errorf("bootstrap seed lacks scope %s", name))
			}
			if err := m.stores[name].RestoreReplica(ctx, state); err != nil {
				return m.fail(err)
			}
			if _, err := m.stores[name].ApplyReplica(ctx, log.Index, dhcp.ReplicaCommand{Operation: "noop", At: cmd.Lease.At}); err != nil {
				return m.fail(err)
			}
		}
		m.lastTime.Store(cmd.Lease.At.Unix())
		m.appliedIndex.Store(log.Index)
		m.initialized.Store(true)
		return applyResult{}
	}
	if !m.initialized.Load() {
		return m.fail(errors.New("lease command precedes bootstrap seed"))
	}
	store := m.stores[cmd.Scope]
	if store == nil {
		return m.fail(fmt.Errorf("committed command names unknown scope %q", cmd.Scope))
	}
	// A new leader may receive work while previously committed entries are
	// still reaching its FSM. Clamp in the deterministic apply path as well as
	// at proposal time, so no replica can move allocation time backward.
	if last := m.lastTime.Load(); cmd.Lease.At.Unix() < last {
		cmd.Lease.At = time.Unix(last, 0).UTC()
	}
	result, err := store.ApplyReplica(ctx, log.Index, cmd.Lease)
	var storage *dhcp.ReplicaStorageError
	if errors.As(err, &storage) {
		return m.fail(err)
	}
	if cmd.Lease.At.Unix() > m.lastTime.Load() {
		m.lastTime.Store(cmd.Lease.At.Unix())
	}
	m.appliedIndex.Store(log.Index)
	return applyResult{Result: result, Err: err}
}

type snapshotData struct {
	Version      int               `json:"version"`
	Fingerprint  string            `json:"fingerprint"`
	Initialized  bool              `json:"initialized"`
	LastTime     int64             `json:"lastTime"`
	AppliedIndex uint64            `json:"appliedIndex"`
	Stores       map[string][]byte `json:"stores"`
}

func (m *machine) Snapshot() (raft.FSMSnapshot, error) {
	if f := m.failed.Load(); f != nil {
		return nil, f.err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	d := snapshotData{Version: 1, Fingerprint: m.fingerprint, Initialized: m.initialized.Load(), LastTime: m.lastTime.Load(), AppliedIndex: m.appliedIndex.Load(), Stores: map[string][]byte{}}
	for _, name := range m.names {
		b, err := m.stores[name].CaptureReplica(ctx)
		if err != nil {
			return nil, err
		}
		d.Stores[name] = b
	}
	b, err := json.Marshal(d)
	if err != nil {
		return nil, err
	}
	if len(b) > maxSnapshotBytes {
		return nil, errors.New("dhcp HA: snapshot exceeds size limit")
	}
	return &snapshot{data: b}, nil
}
func (m *machine) Restore(r io.ReadCloser) error {
	defer func() { _ = r.Close() }()
	b, err := io.ReadAll(io.LimitReader(r, maxSnapshotBytes+1))
	if err != nil {
		return err
	}
	if len(b) > maxSnapshotBytes {
		return errors.New("dhcp HA: incoming snapshot exceeds size limit")
	}
	var d snapshotData
	if err := json.Unmarshal(b, &d); err != nil {
		return err
	}
	if d.Version != 1 || d.Fingerprint != m.fingerprint || len(d.Stores) != len(m.stores) {
		return errors.New("dhcp HA: snapshot cluster/topology mismatch")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if d.Initialized && m.markStarted != nil {
		if err := m.markStarted(); err != nil {
			return err
		}
	}
	for _, name := range m.names {
		data, ok := d.Stores[name]
		if !ok {
			return errors.New("dhcp HA: snapshot missing scope")
		}
		if err := m.stores[name].RestoreReplica(ctx, data); err != nil {
			m.fail(err)
			return err
		}
	}
	m.lastTime.Store(d.LastTime)
	m.appliedIndex.Store(d.AppliedIndex)
	m.initialized.Store(d.Initialized)
	return nil
}

type snapshot struct{ data []byte }

func (s *snapshot) Persist(sink raft.SnapshotSink) error {
	if _, err := sink.Write(s.data); err != nil {
		_ = sink.Cancel()
		return err
	}
	return sink.Close()
}
func (s *snapshot) Release() { s.data = nil }
