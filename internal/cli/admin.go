package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"
)

// DefaultAdminSocket is where `kudurru serve` listens for admin commands
// (docs/architecture.md §8.7: SO_PEERCRED uid 0, the server's own uid, or
// a configured gid).
const DefaultAdminSocket = "/run/kudurru/kudurru.sock"

// AdminRequest is one line sent to the admin socket.
type AdminRequest struct {
	Op   string `json:"op"`
	Args any    `json:"args,omitempty"`
}

// AdminResponse is one line received from the admin socket.
type AdminResponse struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  json.RawMessage `json:"error,omitempty"`
}

// AdminError is a refused admin operation.
type AdminError struct {
	Op      string
	Key     string
	Message string
}

func (e *AdminError) Error() string {
	switch {
	case e.Key != "" && e.Message != "":
		return fmt.Sprintf("admin %s: %s: %s", e.Op, e.Key, e.Message)
	case e.Message != "":
		return fmt.Sprintf("admin %s: %s", e.Op, e.Message)
	case e.Key != "":
		return fmt.Sprintf("admin %s: %s", e.Op, e.Key)
	}
	return fmt.Sprintf("admin %s: refused", e.Op)
}

// AdminCall sends one request over the unix socket and decodes the result
// into out (nil discards it). The protocol is newline-delimited JSON over
// SOCK_STREAM: the client writes {op, args}\n and reads {ok, result,
// error}\n. error may be a string or {key, message}.
func AdminCall(ctx context.Context, socket, op string, args, out any) error {
	if socket == "" {
		socket = DefaultAdminSocket
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		return fmt.Errorf("admin socket %s: %w (run this inside the kudurru container or host, as root or the server's user)", socket, err)
	}
	defer func() { _ = conn.Close() }()
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(30 * time.Second)
	}
	_ = conn.SetDeadline(deadline)
	line, err := json.Marshal(AdminRequest{Op: op, Args: args})
	if err != nil {
		return err
	}
	if _, err := conn.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("admin socket: write: %w", err)
	}
	r := bufio.NewReader(conn)
	resp, err := r.ReadBytes('\n')
	if err != nil && len(resp) == 0 {
		return fmt.Errorf("admin socket: read: %w", err)
	}
	var ar AdminResponse
	if err := json.Unmarshal(resp, &ar); err != nil {
		return fmt.Errorf("admin socket: bad response: %w", err)
	}
	if !ar.OK {
		return decodeAdminError(op, ar.Error)
	}
	if out == nil || len(ar.Result) == 0 {
		return nil
	}
	return json.Unmarshal(ar.Result, out)
}

func decodeAdminError(op string, raw json.RawMessage) error {
	e := &AdminError{Op: op}
	if len(raw) == 0 || string(raw) == "null" {
		return e
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		e.Message = s
		return e
	}
	var obj struct {
		Key     string `json:"key"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return errors.Join(e, err)
	}
	e.Key, e.Message = obj.Key, obj.Message
	return e
}
