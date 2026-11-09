package api

import (
	"bytes"
	"context"
	"encoding/json"
)

// ForwardedZone is one zone convenience command (record upsert/delete, import,
// zone delete) that an enrolled SQLite follower hands to its original primary.
// Input is the route's own decoded input; the primary decodes, validates and
// plans it again in its actual transaction. It carries no permission.
type ForwardedZone struct {
	Action ZoneCommandAction `json:"action"`
	Zone   string            `json:"zone"`
	Input  json.RawMessage   `json:"input"`
}

// ZoneForward hands one zone command to the enrolled primary. handled=false
// means this node runs its own current command; handled=true never falls back.
// It returns the route's exact output document on success.
type ZoneForward func(context.Context, ForwardedZone) (json.RawMessage, bool, error)

// ZoneExecute runs one zone plan inside the caller's actual transaction with
// the same admission as ZoneCommand. Only a primary member executor supplies it.
type ZoneExecute func(context.Context, ZoneCommandTarget, ZoneCommandPrepare) error

type zoneExecuteKey struct{}

func forwardZone[I, O any](ctx context.Context, s *Server, action ZoneCommandAction, zone string, in *I, local func(context.Context, *I) (*O, error)) (*O, error) {
	if s.opts.ZoneForward != nil && ctx.Value(zoneExecuteKey{}) == nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		out, handled, err := s.opts.ZoneForward(ctx, ForwardedZone{Action: action, Zone: zone, Input: raw})
		if handled {
			if err != nil {
				return nil, storeError(err)
			}
			result := new(O)
			d := json.NewDecoder(bytes.NewReader(out))
			d.DisallowUnknownFields()
			if d.Decode(result) != nil || d.More() {
				return nil, errUnavailable("primary returned an invalid zone command result")
			}
			return result, nil
		}
	}
	return local(ctx, in)
}

func decodeForwardedInput(raw json.RawMessage, out any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if len(raw) == 0 || d.Decode(out) != nil || d.More() {
		return errInvalid("forwarded zone command: invalid input")
	}
	return nil
}

// ForwardedZoneCommand runs one forwarded zone command through the same route
// function as the primary's own API, with every plan executed by exec inside
// the member command's transaction. It returns that route's output document.
func (s *Server) ForwardedZoneCommand(ctx context.Context, in ForwardedZone, exec ZoneExecute) (json.RawMessage, error) {
	if s == nil || exec == nil || s.opts.ZoneCommand == nil {
		return nil, errUnavailable("zone command forwarding unavailable")
	}
	ctx = context.WithValue(ctx, zoneExecuteKey{}, exec)
	var out any
	var err error
	switch in.Action {
	case ZoneCommandUpsertRecord:
		var input upsertRecordInput
		if err := decodeForwardedInput(in.Input, &input); err != nil || input.Zone != in.Zone {
			return nil, errInvalid("forwarded zone command: invalid input")
		}
		out, err = s.commandUpsertZoneRecord(ctx, &input)
	case ZoneCommandDeleteRecord:
		var input deleteRecordInput
		if err := decodeForwardedInput(in.Input, &input); err != nil || input.Zone != in.Zone {
			return nil, errInvalid("forwarded zone command: invalid input")
		}
		out, err = s.commandDeleteZoneRecord(ctx, &input)
	case ZoneCommandImport:
		var input importInput
		if err := decodeForwardedInput(in.Input, &input); err != nil || input.Zone != in.Zone {
			return nil, errInvalid("forwarded zone command: invalid input")
		}
		out, err = s.commandImportZone(ctx, &input)
	case ZoneCommandDeleteZone:
		var input deleteZoneInput
		if err := decodeForwardedInput(in.Input, &input); err != nil || input.Zone != in.Zone {
			return nil, errInvalid("forwarded zone command: invalid input")
		}
		out, err = s.commandDeleteZone(ctx, &input)
	default:
		return nil, errInvalid("forwarded zone command: unsupported action")
	}
	if err != nil {
		return nil, err
	}
	return json.Marshal(out)
}
