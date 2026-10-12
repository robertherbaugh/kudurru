package config

import (
	"errors"
	"net/url"
	"strings"
)

// Database configures the optional central PostgreSQL database of runtime
// data: audit events, query log and statistics of every node
// (internal/central, D158). Configuration objects never go there.
type Database struct {
	// DSN is the connection string; empty turns the database off. It is
	// hostOnly and writeOnly (it may hold a password): set it in the file
	// or with KUDURRU_DATABASE__DSN_FILE.
	DSN string `koanf:"dsn" json:"dsn"`
	// MaxConns caps the node's connections.
	MaxConns int `koanf:"maxConns" json:"maxConns"`
}

// Enabled reports whether a central database is configured.
func (d Database) Enabled() bool { return d.DSN != "" }

// validate checks the DSN's shape without echoing it.
func (d Database) validate() []error {
	if d.DSN == "" {
		return nil
	}
	dsn := strings.TrimSpace(d.DSN)
	if strings.Contains(dsn, "://") {
		u, err := url.Parse(dsn)
		if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
			return []error{errors.New("database.dsn: not a postgres:// or postgresql:// URL")}
		}
		return nil
	}
	if !strings.Contains(dsn, "=") {
		return []error{errors.New("database.dsn: not a PostgreSQL connection string (a postgres:// URL or key=value pairs)")}
	}
	return nil
}
