package sqlite

import (
	"context"
	"encoding/json"
	"unicode/utf8"

	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

func validSectionPrefix(section, prefix string) bool {
	return replication.Section(&replication.Payload{}, section) != nil && len(prefix) > 0 && len(prefix) <= 512 && utf8.ValidString(prefix)
}

// CheckSectionPrefixBounds checks scalar budgets before allocating row payloads.
// It sees earlier staged changes in this command. Prefix is a literal UTF-8 byte
// prefix of at most 512 bytes; %, _ and backslash have no wildcard meaning.
// As with CheckSectionBounds, zero budgets allow an empty result, protocol image
// caps bound requested budgets, and every failure poisons the command.
func (t *CommandTx) CheckSectionPrefixBounds(ctx context.Context, section, prefix string, maxRows, maxDataBytes int) error {
	if err := t.valid(ctx); err != nil {
		return err
	}
	limits := (replication.Limits{}).Defaults()
	if !validSectionPrefix(section, prefix) || maxRows < 0 || maxRows > limits.MaxEntries || maxDataBytes < 0 || int64(maxDataBytes) > limits.MaxBytes {
		return t.poison(replication.ErrInvalid)
	}
	var count, size, longestKey int64
	err := t.tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(length(CAST(data AS BLOB))),0),COALESCE(MAX(length(CAST(key AS BLOB))),0) FROM replication_sections WHERE section=? AND substr(CAST(key AS BLOB),1,?)=CAST(? AS BLOB)`, section, len(prefix), prefix).Scan(&count, &size, &longestKey)
	if err != nil {
		return t.poison(err)
	}
	if count > int64(maxRows) || size > int64(maxDataBytes) || longestKey > 512 {
		return t.poison(replication.ErrTooLarge)
	}
	return nil
}

// ListSectionPrefix reads only literal matching rows ordered by key in this
// command, including staged changes. Call CheckSectionPrefixBounds first to
// enforce the owner's aggregate budget before allocating. Like ListSections,
// this returns raw owner payloads without decoding or repairing historical rows;
// the owner must validate them and return any read/decode error from its callback.
func (t *CommandTx) ListSectionPrefix(ctx context.Context, section, prefix string) ([]replication.Entry, error) {
	if err := t.valid(ctx); err != nil {
		return nil, err
	}
	if !validSectionPrefix(section, prefix) {
		return nil, replication.ErrInvalid
	}
	rows, err := t.tx.QueryContext(ctx, `SELECT key,data FROM replication_sections WHERE section=? AND substr(CAST(key AS BLOB),1,?)=CAST(? AS BLOB) ORDER BY key`, section, len(prefix), prefix)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	entries := []replication.Entry{}
	for rows.Next() {
		var key, raw string
		if err := rows.Scan(&key, &raw); err != nil {
			return nil, err
		}
		entries = append(entries, replication.Entry{Key: key, Data: json.RawMessage(raw)})
	}
	return entries, rows.Err()
}
