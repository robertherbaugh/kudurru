package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/robertherbaugh/kudurru/internal/apiclient"
)

// DefaultAdminSocket is where `kudurru serve` listens for admin commands
// (dev-docs/design-record.md §8.7: SO_PEERCRED uid 0, the server's own uid, or
// a configured gid).
const DefaultAdminSocket = "/run/kudurru/kudurru.sock"

// Admin operations.
const (
	AdminResetPassword = "reset-password"
	AdminCreateToken   = "create-token"
	AdminListUsers     = "list-users"
)

// Admin socket protocols.
const (
	// AdminProtocolHTTP is HTTP/1.1 with JSON bodies over the socket:
	// POST /admin/v1/reset-password, POST /admin/v1/create-token, GET
	// /admin/v1/list-users; errors are the API's {error:{key,message,hint}}.
	// This is what internal/auth/adminsock serves.
	AdminProtocolHTTP = "http"
	// AdminProtocolNDJSON is one line {op, args} answered by one line
	// {ok, result, error}: the fallback protocol of the M3 work package.
	AdminProtocolNDJSON = "ndjson"
	// AdminProtocolAuto tries HTTP and falls back to NDJSON when the
	// server does not answer HTTP.
	AdminProtocolAuto = "auto"
)

// AdminClient calls the break-glass admin socket.
type AdminClient struct {
	// Socket is the unix socket path (default DefaultAdminSocket).
	Socket string
	// Protocol is http, ndjson or auto (default auto).
	Protocol string
	// Timeout bounds one call when the context has no deadline (30 s).
	Timeout time.Duration
}

// AdminRequest is one NDJSON request line.
type AdminRequest struct {
	Op   string `json:"op"`
	Args any    `json:"args,omitempty"`
}

// AdminResponse is one NDJSON response line.
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

// errNotHTTP marks a server that does not speak HTTP on the socket.
var errNotHTTP = errors.New("admin socket does not speak HTTP")

// Call runs op with args (marshalled as the JSON body) and decodes the
// result into out (nil discards it). Refusals are *AdminError.
func (a *AdminClient) Call(ctx context.Context, op string, args, out any) error {
	socket := a.Socket
	if socket == "" {
		socket = DefaultAdminSocket
	}
	if _, ok := ctx.Deadline(); !ok {
		timeout := a.Timeout
		if timeout == 0 {
			timeout = 30 * time.Second
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	switch a.Protocol {
	case AdminProtocolNDJSON:
		return callNDJSON(ctx, socket, op, args, out)
	case AdminProtocolHTTP:
		return callHTTP(ctx, socket, op, args, out)
	case "", AdminProtocolAuto:
		err := callHTTP(ctx, socket, op, args, out)
		if errors.Is(err, errNotHTTP) {
			return callNDJSON(ctx, socket, op, args, out)
		}
		return err
	}
	return fmt.Errorf("admin: unknown protocol %q (http, ndjson, auto)", a.Protocol)
}

func dialHint(socket string, err error) error {
	return fmt.Errorf("admin socket %s: %w (run this inside the kudurru container or host, as root or the server's user)", socket, err)
}

func callHTTP(ctx context.Context, socket, op string, args, out any) error {
	var (
		d       net.Dialer
		mu      sync.Mutex
		dialErr error
	)
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			c, err := d.DialContext(ctx, "unix", socket)
			mu.Lock()
			dialErr = err
			mu.Unlock()
			return c, err
		},
		DisableKeepAlives: true,
	}
	defer tr.CloseIdleConnections()
	method := http.MethodPost
	if op == AdminListUsers {
		method = http.MethodGet
	}
	var body io.Reader
	if args != nil && method == http.MethodPost {
		b, err := json.Marshal(args)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	} else if method == http.MethodPost {
		body = strings.NewReader("{}")
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://kudurru/admin/v1/"+op, body)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		mu.Lock()
		de := dialErr
		mu.Unlock()
		if de != nil {
			return dialHint(socket, de)
		}
		if ctx.Err() != nil {
			return err
		}
		// Connected, but the answer was not HTTP (an NDJSON server
		// answers a line of JSON or hangs up).
		return fmt.Errorf("%w: %v", errNotHTTP, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if err := apiclient.CheckResponse(resp); err != nil {
		var ae *apiclient.Error
		if errors.As(err, &ae) {
			return &AdminError{Op: op, Key: ae.Key, Message: ae.Message}
		}
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("admin %s: bad response: %w", op, err)
	}
	return nil
}

func callNDJSON(ctx context.Context, socket, op string, args, out any) error {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		return dialHint(socket, err)
	}
	defer func() { _ = conn.Close() }()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	line, err := json.Marshal(AdminRequest{Op: op, Args: args})
	if err != nil {
		return err
	}
	if _, err := conn.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("admin socket: write: %w", err)
	}
	resp, err := bufio.NewReader(conn).ReadBytes('\n')
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
