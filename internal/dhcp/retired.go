package dhcp

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"time"
)

// LockServiceDirectory prevents services using different scope names from
// concurrently allocating from independent databases in the same directory.
func LockServiceDirectory(dir string) (io.Closer, error) {
	lock, err := lockLeaseFile(filepath.Join(dir, "dhcp.lock"))
	if err != nil {
		return nil, fmt.Errorf("dhcp: acquire service directory: %w", err)
	}
	return lock, nil
}

// CheckRetiredDatabase checks a removed scope without altering its database.
// A live allocation, unknown schema or another owner prevents retirement.
// The returned lock must stay held until the service has stopped.
func CheckRetiredDatabase(path string) (io.Closer, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	lock, err := lockLeaseFile(path)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (io.Closer, error) {
		_ = lock.Close()
		return nil, err
	}
	u := &url.URL{Scheme: "file", Path: path}
	u.RawQuery = url.Values{"mode": {"ro"}, "_pragma": {"query_only(ON)"}}.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return fail(err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fail(err)
	}
	defer func() { _ = tx.Rollback() }()
	version, err := leaseReaderSchema(ctx, tx)
	if err != nil {
		return fail(err)
	}
	if version == 0 {
		// An interrupted first initialization has no allocations to retire.
		// Keep its inode locked just like an initialized, empty database.
		return lock, nil
	}
	var active int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM leases WHERE expires_at > ?", time.Now().Unix()).Scan(&active); err != nil {
		return fail(err)
	}
	if active != 0 {
		return fail(fmt.Errorf("%d live allocations remain; retain the original scope until its leases and holds expire", active))
	}
	return lock, nil
}
