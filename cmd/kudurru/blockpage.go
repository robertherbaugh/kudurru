package main

import (
	"context"
	"log/slog"
	"net"
	"path/filepath"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/robertherbaugh/kudurru/ee/branding"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/blockpage"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/license"
	"github.com/robertherbaugh/kudurru/internal/querylog"
	"github.com/robertherbaugh/kudurru/internal/querylog/sqlitelog"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/unblock"
)

// querylogPrivacy reads querylog.privacy and whether querylog.anonymize is
// pseudonym from the live settings; an unknown mode reads as the
// strictest.
func querylogPrivacy(reload *config.Reloadable) func() (querylog.Mode, bool) {
	return func() (querylog.Mode, bool) {
		q := reload.Current().Querylog
		m, err := querylog.ParseMode(q.Privacy)
		if err != nil {
			m = querylog.PrivacyNothing
		}
		return m, q.Anonymize == querylog.AnonymizePseudonym
	}
}

// newUnblockBook is the unblock request book (internal/unblock): requests
// in the selected namespace, the client recorded as querylog.privacy
// allows, object names keyed by the encryption key.
func newUnblockBook(st store.Store, reload *config.Reloadable, namespace, node string, encKey []byte, logger *slog.Logger) (*unblock.Service, error) {
	nameKey, err := unblock.NameKey(encKey)
	if err != nil {
		return nil, err
	}
	return unblock.New(unblock.Options{
		Store: st, Namespace: namespace, Node: node,
		Settings: func() config.UnblockRequests { return reload.Current().UnblockRequests },
		Privacy:  querylogPrivacy(reload),
		NameKey:  nameKey,
		Logger:   logger,
	}), nil
}

// Self-signed block page certificate under <node.dataDir>/keys/ when
// listeners.blockPage.https has no tls.certFile.
const (
	blockPageCertFile = "blockpage.crt"
	blockPageKeyFile  = "blockpage.key"
)

// newBlockPage builds the block page server. The renderer always exists
// (the API preview works with the block page off); the listeners are only
// configured, and the HTTPS certificate generated, when blockPage.enabled.
// Reasons come from the query log (live tail, then querylog.db) and, when
// it has none, from the client's current policy. ql is the query log's
// collector (nil when it is off); it names the client's pseudonym.
func newBlockPage(cfg *config.Config, reload *config.Reloadable, src blockpage.IndexSource, tail *querylog.Tail, reader *sqlitelog.Reader,
	ql *querylog.Async, book *unblock.Service, lic *license.Manager, reg prometheus.Registerer, logger *slog.Logger, brandSource ...func(context.Context) (*blockpage.Branding, error)) (*blockpage.Server, error) {
	pf := blockpage.PolicyFinder{Source: src}
	qf := &blockpage.QuerylogFinder{
		Tail: tail, Reader: reader, Disabled: !cfg.Querylog.Enabled,
		Privacy: querylogPrivacy(reload),
	}
	if ql != nil {
		qf.Pseudonym = ql.Pseudonym
	}
	o := blockpage.Options{
		Node: cfg.Node.Name, Finder: blockpage.Chain{qf, pf}, Policy: pf.Policy, Membership: pf.Membership,
		Settings: blockpage.SettingsFrom(cfg), Registerer: reg, Logger: logger,
	}
	var brandCache branding.PageCache
	o.Branding = func(ctx context.Context) (*blockpage.Branding, error) {
		cut := lic.Capture()
		if !cut.Status().Allows(license.FeatureBranding) || !cut.Current() {
			return nil, nil
		}
		prepared, err := brandCache.Prepare(ctx, reload.Current().Branding)
		if err != nil || !cut.Current() {
			return nil, err
		}
		return prepared, nil
	}
	if len(brandSource) != 0 {
		o.Branding = brandSource[0]
	}
	if book != nil {
		o.Requests = book
		o.RequestsAllowed = func() bool { return lic.Allows(license.FeatureRequests) }
	}
	if cfg.BlockPage.Enabled {
		l := cfg.Listeners.BlockPage
		o.HTTP, o.HTTPS = l.HTTP, l.HTTPS
		if l.HTTPS != "" {
			o.CertFile, o.KeyFile = l.TLS.CertFile, l.TLS.KeyFile
			if o.CertFile == "" {
				dir := filepath.Join(cfg.Node.DataDir, "keys")
				o.CertFile, o.KeyFile = filepath.Join(dir, blockPageCertFile), filepath.Join(dir, blockPageKeyFile)
				var ips []net.IP
				v4, v6 := cfg.BlockPageAddrs()
				if v4.IsValid() {
					ips = append(ips, v4.AsSlice())
				}
				if v6.IsValid() {
					ips = append(ips, v6.AsSlice())
				}
				if err := api.EnsureSelfSignedPair(o.CertFile, o.KeyFile, cfg.Node.Name, ips...); err != nil {
					return nil, err
				}
				logger.Info("block page: serving a self-signed certificate on HTTPS; browsers warn before showing it", "cert", o.CertFile)
			}
		}
	}
	return blockpage.New(o)
}
