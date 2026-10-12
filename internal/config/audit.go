package config

import (
	"errors"
	"time"
)

// Audit configures where audit events are kept (docs/architecture.md
// §11.4, D157). Both keys are hostOnly: whoever can change settings
// through the API must not be able to move or shorten the audit trail.
type Audit struct {
	// Store is auto, auditlog or local; Store resolves auto.
	Store string `koanf:"store" json:"store"`
	// Retention is how long stored events are kept.
	Retention Duration `koanf:"retention" json:"retention"`
}

// Audit stores (audit.store).
const (
	AuditStoreAuto     = "auto"
	AuditStoreAuditLog = "auditlog"
	AuditStoreLocal    = "local"
)

// minAuditRetention bounds audit.retention below.
const minAuditRetention = Duration(24 * time.Hour)

// AuditStore is the effective audit.store: auto is auditlog with the kube
// store driver and local otherwise.
func (c *Config) AuditStore() string {
	if c.Audit.Store != AuditStoreAuto && c.Audit.Store != "" {
		return c.Audit.Store
	}
	if c.Store.Driver == "kube" {
		return AuditStoreAuditLog
	}
	return AuditStoreLocal
}

func (c *Config) validateAudit() []error {
	var errs []error
	if c.Audit.Retention < minAuditRetention {
		errs = append(errs, errors.New("audit.retention: at least 24h"))
	}
	if c.Audit.Store == AuditStoreAuditLog && c.Store.Driver != "kube" {
		errs = append(errs, errors.New("audit.store: auditlog needs store.driver: kube (AuditLog objects live in the apiserver)"))
	}
	return errs
}
