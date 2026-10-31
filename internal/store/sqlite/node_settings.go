package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
)

// Local node preferences never enter objects, replication_sections, exported
// images, revisions or object snapshots. The version belongs to this record,
// independently of the canonical store/replication schema.
const nodeLogMeta = "local_node_log_v1"
const maxNodeLogBytes = 2048

// NodeLogSettings is a bounded local overlay for exactly one node. Nil leaves
// delegate to host defaults; they are not persisted schema-default overrides.
// The original node name is always supplied by the host, never by a PATCH body.
type NodeLogSettings struct {
	Version int     `json:"version"`
	Node    string  `json:"node"`
	Level   *string `json:"level,omitempty"`
	Format  *string `json:"format,omitempty"`
}

func (n NodeLogSettings) validate() error {
	if n.Version != 1 || n.Node == "" || len(n.Node) > 512 || strings.ContainsAny(n.Node, "\x00\r\n") {
		return errors.New("invalid local node log identity/version")
	}
	if n.Level != nil {
		if len(*n.Level) > 256 {
			return errors.New("local log.level exceeds its bound")
		}
		switch strings.SplitN(*n.Level, ",", 2)[0] {
		case "debug", "info", "warn", "error":
		default:
			return errors.New("invalid local log.level")
		}
	}
	if n.Format != nil && *n.Format != "json" && *n.Format != "text" {
		return errors.New("invalid local log.format")
	}
	return nil
}

func readNodeLog(ctx context.Context, q queryer, node string) (NodeLogSettings, error) {
	empty := NodeLogSettings{Version: 1, Node: node}
	if err := empty.validate(); err != nil {
		return NodeLogSettings{}, err
	}
	var size int
	var raw string
	err := q.QueryRowContext(ctx, `SELECT length(CAST(value AS BLOB)),CASE WHEN length(CAST(value AS BLOB))<=? THEN value ELSE '' END FROM meta WHERE key=?`, maxNodeLogBytes, nodeLogMeta).Scan(&size, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return empty, nil
	}
	if err != nil {
		return NodeLogSettings{}, err
	}
	if size > maxNodeLogBytes {
		return NodeLogSettings{}, errors.New("local node log record exceeds its bound")
	}
	var n NodeLogSettings
	dec := json.NewDecoder(bytes.NewBufferString(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&n); err != nil {
		return NodeLogSettings{}, errors.New("invalid local node log document")
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return NodeLogSettings{}, errors.New("invalid trailing local node log document")
	}
	if err := n.validate(); err != nil {
		return NodeLogSettings{}, err
	}
	if n.Node != node {
		return empty, nil
	}
	return n, nil
}

// ReadNodeLogSettings reads only committed node-local metadata before logger
// construction. It never creates a missing database or acquires a writer; an
// uninitialized database has no local overlay. The caller bounds startup time.
func ReadNodeLogSettings(ctx context.Context, path, node string) (NodeLogSettings, error) {
	if err := ctx.Err(); err != nil {
		return NodeLogSettings{}, err
	}
	empty := NodeLogSettings{Version: 1, Node: node}
	if err := empty.validate(); err != nil {
		return NodeLogSettings{}, err
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return empty, nil
	} else if err != nil {
		return NodeLogSettings{}, err
	}
	params := url.Values{"mode": {"ro"}, "_pragma": {"query_only(1)", "busy_timeout(1000)"}}
	db, err := sql.Open("sqlite", "file:"+(&url.URL{Path: path}).EscapedPath()+"?"+params.Encode())
	if err != nil {
		return NodeLogSettings{}, err
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	var exists bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='meta')`).Scan(&exists); err != nil {
		return NodeLogSettings{}, err
	}
	if !exists {
		return empty, nil
	}
	return readNodeLog(ctx, db, node)
}

// NodeLogSettings reads the overlay in this command's original transaction.
func (t *CommandTx) NodeLogSettings(ctx context.Context, node string) (NodeLogSettings, error) {
	if err := t.valid(ctx); err != nil {
		return NodeLogSettings{}, err
	}
	n, err := readNodeLog(ctx, t.tx, node)
	return n, t.poison(err)
}

// PutNodeLogSettings stages only this typed local metadata. Final admission and
// rollback are owned by the same concrete Command transaction as authorization.
// No nested write, canonical revision or replica publication is introduced.
func (t *CommandTx) PutNodeLogSettings(ctx context.Context, n NodeLogSettings) error {
	if err := t.valid(ctx); err != nil {
		return err
	}
	if err := n.validate(); err != nil {
		return t.poison(err)
	}
	var result sql.Result
	var err error
	if n.Level == nil && n.Format == nil {
		// A reset for a different host identity cannot erase its dormant overlay.
		result, err = t.tx.ExecContext(ctx, `DELETE FROM meta WHERE key=? AND json_extract(value,'$.node')=?`, nodeLogMeta, n.Node)
	} else {
		raw, marshalErr := json.Marshal(n)
		if marshalErr != nil {
			return t.poison(marshalErr)
		}
		if len(raw) > maxNodeLogBytes {
			return t.poison(fmt.Errorf("local node log record exceeds %d bytes", maxNodeLogBytes))
		}
		result, err = t.tx.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value WHERE meta.value!=excluded.value`, nodeLogMeta, string(raw))
	}
	if err != nil {
		return t.poison(err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return t.poison(err)
	}
	t.localChanged = t.localChanged || count > 0
	return nil
}
