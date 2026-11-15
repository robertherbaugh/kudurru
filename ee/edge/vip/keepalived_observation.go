package vip

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"syscall"
	"time"

	"github.com/robertherbaugh/kudurru/ee/edge/keepalived"
)

// KeepalivedNotice describes an observed transition or an explicit acquisition
// issue. Session/Sequence are source evidence, never current writer authority.
type KeepalivedNotice struct {
	Event    *Event
	Issue    string
	Session  string
	Sequence uint64
}

// Observe reads the existing Consumer's bounded journal without opening its
// FIFO, starting keepalived or accessing track-file writers. The first valid
// snapshot and every new source session establish a baseline, never a replay
// or an invented previous-state transition. Legacy files remain readable by
// existing owners but cannot establish a notification cursor here.
func (k *Keepalived) Observe(ctx context.Context, maxAge time.Duration, emit func(context.Context, KeepalivedNotice)) error {
	if k.Node == "" || emit == nil || maxAge < time.Second || maxAge > time.Minute {
		return errors.New("vip observation: node, bounded age and sink required")
	}
	poll := k.Poll
	if poll <= 0 {
		poll = time.Second
	}
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	var session, lastIssue string
	var sequence uint64
	issue := func(reason string) {
		if reason != lastIssue {
			emit(ctx, KeepalivedNotice{Issue: reason, Session: session, Sequence: sequence})
			lastIssue = reason
		}
	}
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		state, err := readObservationJournal(k.RunDir)
		now := time.Now()
		switch {
		case err != nil:
			issue("source_unavailable")
		case state.Node != k.Node:
			issue("source_node_mismatch")
		case state.Session == "":
			issue("source_legacy")
		case !validJournal(state):
			issue("source_invalid")
		case session == "" || session != state.Session:
			reason := "source_ready"
			if session != "" {
				reason = "source_restart"
			}
			session, sequence = state.Session, state.Sequence
			lastIssue = ""
			issue(reason)
		case state.Sequence < sequence:
			issue("source_regressed")
		case state.Sequence > sequence:
			if len(state.Transitions) == 0 || state.Transitions[0].Sequence > sequence+1 {
				sequence = state.Sequence
				lastIssue = ""
				issue("journal_gap")
				break
			}
			for _, transition := range state.Transitions {
				if transition.Sequence <= sequence {
					continue
				}
				sequence = transition.Sequence
				var selected *VIP
				for i := range k.vips {
					if k.vips[i].Name == transition.VIP {
						selected = &k.vips[i]
					}
				}
				if selected == nil {
					continue
				}
				at := transition.Next.Since
				if at.After(now) || now.Sub(at) > maxAge {
					issue("source_stale")
					continue
				}
				event := &Event{VIP: selected.Name, Node: k.Node, VRID: selected.VRID, Previous: State(transition.Previous.State), State: State(transition.Next.State), Priority: transition.Next.Priority, At: at}
				emit(ctx, KeepalivedNotice{Event: event, Session: session, Sequence: sequence})
				lastIssue = ""
			}
		case lastIssue != "" && lastIssue != "source_ready":
			issue("source_ready")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func validJournal(state keepalived.StateFile) bool {
	if len(state.Session) > 64 || len(state.Transitions) > keepalived.TransitionLimit || len(state.VIPs) > 64 {
		return false
	}
	if len(state.Transitions) == 0 {
		return state.Sequence == 0
	}
	var previous uint64
	for i, item := range state.Transitions {
		if item.Sequence == 0 || i > 0 && item.Sequence != previous+1 || item.VIP == "" || len(item.VIP) > 31 || item.Next.Since.IsZero() || item.Next.Since.Before(item.Previous.Since) || !vrrpState(item.Next.State) || item.Previous.State != "" && !vrrpState(item.Previous.State) || item.Next.State == item.Previous.State || item.Next.Priority < 0 || item.Next.Priority > 255 {
			return false
		}
		previous = item.Sequence
	}
	return previous == state.Sequence
}

func vrrpState(state string) bool {
	return state == keepalived.StateMaster || state == keepalived.StateBackup || state == keepalived.StateFault || state == keepalived.StateStop
}

// Refuse FIFOs/devices/symlinks, cap bytes, and never turn a failed source read
// into cached ownership. NONBLOCK avoids a malicious FIFO blocking at open.
func readObservationJournal(dir string) (keepalived.StateFile, error) {
	f, err := os.OpenFile(keepalived.StatePath(dir), os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0) //nolint:gosec // explicit host-only observation path, symlinks refused
	if err != nil {
		return keepalived.StateFile{}, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return keepalived.StateFile{}, errors.New("vip observation: invalid journal file")
	}
	data, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return keepalived.StateFile{}, errors.New("vip observation: journal read failed")
	}
	var state keepalived.StateFile
	if err := json.Unmarshal(data, &state); err != nil {
		return keepalived.StateFile{}, errors.New("vip observation: invalid journal encoding")
	}
	return state, nil
}
