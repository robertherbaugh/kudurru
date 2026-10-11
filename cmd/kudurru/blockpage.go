package main

import (
	"log/slog"
	"net"
	"path/filepath"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/blockpage"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/querylog"
	"github.com/robertherbaugh/kudurru/internal/querylog/sqlitelog"
)

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
// it has none, from the client's current policy.
func newBlockPage(cfg *config.Config, reload *config.Reloadable, src blockpage.IndexSource, tail *querylog.Tail, reader *sqlitelog.Reader,
	reg prometheus.Registerer, logger *slog.Logger) (*blockpage.Server, error) {
	pf := blockpage.PolicyFinder{Source: src}
	qf := &blockpage.QuerylogFinder{
		Tail: tail, Reader: reader, Disabled: !cfg.Querylog.Enabled,
		Privacy: func() (querylog.Mode, bool) {
			q := reload.Current().Querylog
			m, err := querylog.ParseMode(q.Privacy)
			if err != nil {
				m = querylog.PrivacyNothing
			}
			return m, q.Anonymize == querylog.AnonymizePseudonym
		},
	}
	o := blockpage.Options{
		Node: cfg.Node.Name, Finder: blockpage.Chain{qf, pf}, Policy: pf.Policy,
		Settings: blockpage.SettingsFrom(cfg), Registerer: reg, Logger: logger,
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
