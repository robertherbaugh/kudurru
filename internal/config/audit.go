package config

import (
	"errors"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"
)

// Audit configures where audit events are kept (dev-docs/design-record.md
// §11.4, D157). These keys are hostOnly: whoever can change settings
// through the API must not be able to move or shorten the audit trail.
type Audit struct {
	// KeySecret is the operator-provisioned independent AuditLog signing ring.
	KeySecret string `koanf:"keySecret" json:"keySecret"`
	// Store is auto, database, auditlog or local; AuditStore resolves
	// auto.
	Store string `koanf:"store" json:"store"`
	// Retention is how long stored events are kept.
	Retention Duration `koanf:"retention" json:"retention"`
}

// Audit stores (audit.store).
const (
	AuditStoreAuto     = "auto"
	AuditStoreDatabase = "database"
	AuditStoreAuditLog = "auditlog"
	AuditStoreLocal    = "local"
)

// minAuditRetention bounds audit.retention below.
const minAuditRetention = Duration(24 * time.Hour)

// AuditStore is the effective audit.store: auto is database when a central
// database is configured, auditlog with the kube store driver and local
// otherwise.
func (c *Config) AuditStore() string {
	if c.Audit.Store != AuditStoreAuto && c.Audit.Store != "" {
		return c.Audit.Store
	}
	if c.Database.Enabled() {
		return AuditStoreDatabase
	}
	if c.Store.Driver == "kube" {
		return AuditStoreAuditLog
	}
	return AuditStoreLocal
}

func (c *Config) validateAudit() []error {
	var errs []error
	if len(validation.IsDNS1123Subdomain(c.Audit.KeySecret)) != 0 {
		errs = append(errs, errors.New("audit.keySecret: a Kubernetes Secret name is required"))
	}
	if c.Audit.Retention < minAuditRetention {
		errs = append(errs, errors.New("audit.retention: at least 24h"))
	}
	if c.Audit.Store == AuditStoreAuditLog && c.Store.Driver != "kube" {
		errs = append(errs, errors.New("audit.store: auditlog needs store.driver: kube (AuditLog objects live in the apiserver)"))
	}
	if c.Audit.Store == AuditStoreDatabase && !c.Database.Enabled() {
		errs = append(errs, errors.New("audit.store: database needs database.dsn"))
	}
	return errs
}
