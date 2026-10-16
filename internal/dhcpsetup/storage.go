package dhcpsetup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

func readSettings(path string) ([]byte, error) {
	file, err := privateFile(path, false)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, maxSettingsBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxSettingsBytes {
		return nil, fmt.Errorf("%w: saved setup exceeds %d bytes", ErrInvalidConfiguration, maxSettingsBytes)
	}
	return data, nil
}

// writeSettings returns replaced=true only after the atomic rename. If a
// subsequent directory sync fails, durability is unknown and further saves
// must fail closed until reopening the manager from its on-disk state.
func (m *Manager) writeSettings(ctx context.Context, data []byte) (replaced bool, err error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	// Refuse links or special destination files rather than silently replacing
	// them. Parent aliases were resolved once at Open.
	existing, err := privateFile(m.path, false)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if existing != nil {
		if err := existing.Close(); err != nil {
			return false, err
		}
	}
	dir, err := os.Open(filepath.Dir(m.path))
	if err != nil {
		return false, err
	}
	defer func() { _ = dir.Close() }()
	temp, err := os.CreateTemp(filepath.Dir(m.path), ".dhcp-settings-*")
	if err != nil {
		return false, err
	}
	defer func() { _ = temp.Close(); _ = os.Remove(temp.Name()) }()
	if err := temp.Chmod(0o600); err != nil {
		return false, err
	}
	if _, err := temp.Write(data); err != nil {
		return false, err
	}
	if err := m.syncFile(temp); err != nil {
		return false, err
	}
	if err := temp.Close(); err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := m.rename(temp.Name(), m.path); err != nil {
		return false, err
	}
	// After rename, finish the directory sync even if the request is canceled.
	// Returning success then has one clear durable outcome for the caller.
	if err := m.syncFile(dir); err != nil {
		return true, err
	}
	return true, nil
}
