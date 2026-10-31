package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/spf13/cobra"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	"github.com/robertherbaugh/kudurru/dashboard"
	"github.com/robertherbaugh/kudurru/ee/auditstore"
	"github.com/robertherbaugh/kudurru/ee/branding"
	"github.com/robertherbaugh/kudurru/ee/edge/dnsdist"
	"github.com/robertherbaugh/kudurru/ee/integration/externaldns"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/audit"
	"github.com/robertherbaugh/kudurru/internal/audit/auditkeys"
	"github.com/robertherbaugh/kudurru/internal/audit/auditlog"
	"github.com/robertherbaugh/kudurru/internal/audit/spool"
	authn "github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/auth/adminsock"
	"github.com/robertherbaugh/kudurru/internal/auth/iamgate"
	"github.com/robertherbaugh/kudurru/internal/auth/keys"
	"github.com/robertherbaugh/kudurru/internal/auth/session"
	"github.com/robertherbaugh/kudurru/internal/auth/stack"
	"github.com/robertherbaugh/kudurru/internal/auth/users"
	"github.com/robertherbaugh/kudurru/internal/blocking"
	"github.com/robertherbaugh/kudurru/internal/blockpage"
	"github.com/robertherbaugh/kudurru/internal/bootstrap"
	"github.com/robertherbaugh/kudurru/internal/cache"
	"github.com/robertherbaugh/kudurru/internal/cache/persist"
	"github.com/robertherbaugh/kudurru/internal/central"
	"github.com/robertherbaugh/kudurru/internal/central/pgaudit"
	"github.com/robertherbaugh/kudurru/internal/central/pgquerylog"
	"github.com/robertherbaugh/kudurru/internal/cluster"
	"github.com/robertherbaugh/kudurru/internal/cluster/internalapi"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/controller"
	"github.com/robertherbaugh/kudurru/internal/dhcpkube"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/dnsserver/proxyproto"
	"github.com/robertherbaugh/kudurru/internal/domainlist"
	"github.com/robertherbaugh/kudurru/internal/health"
	"github.com/robertherbaugh/kudurru/internal/inventory"
	"github.com/robertherbaugh/kudurru/internal/license"
	"github.com/robertherbaugh/kudurru/internal/listdata/catalogue"
	"github.com/robertherbaugh/kudurru/internal/listfetcher"
	"github.com/robertherbaugh/kudurru/internal/metrics"
	"github.com/robertherbaugh/kudurru/internal/notifier"
	"github.com/robertherbaugh/kudurru/internal/objects"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/policy"
	"github.com/robertherbaugh/kudurru/internal/querylog"
	"github.com/robertherbaugh/kudurru/internal/querylog/sqlitelog"
	"github.com/robertherbaugh/kudurru/internal/recursor"
	"github.com/robertherbaugh/kudurru/internal/rpz"
	"github.com/robertherbaugh/kudurru/internal/runtimeowners"
	"github.com/robertherbaugh/kudurru/internal/secondary"
	"github.com/robertherbaugh/kudurru/internal/servingbuild"
	"github.com/robertherbaugh/kudurru/internal/servingdns"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	"github.com/robertherbaugh/kudurru/internal/settings"
	"github.com/robertherbaugh/kudurru/internal/stages/authoritative"
	blockingstage "github.com/robertherbaugh/kudurru/internal/stages/blocking"
	cachestage "github.com/robertherbaugh/kudurru/internal/stages/cache"
	"github.com/robertherbaugh/kudurru/internal/stages/control"
	dnssecstage "github.com/robertherbaugh/kudurru/internal/stages/dnssec"
	"github.com/robertherbaugh/kudurru/internal/stages/identify"
	"github.com/robertherbaugh/kudurru/internal/stages/ingress"
	logstage "github.com/robertherbaugh/kudurru/internal/stages/log"
	"github.com/robertherbaugh/kudurru/internal/stages/postprocess"
	"github.com/robertherbaugh/kudurru/internal/stages/rewrite"
	upstreamstage "github.com/robertherbaugh/kudurru/internal/stages/upstream"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/kube"
	"github.com/robertherbaugh/kudurru/internal/store/localdb"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
	"github.com/robertherbaugh/kudurru/internal/stub"
	"github.com/robertherbaugh/kudurru/internal/unblock"
	"github.com/robertherbaugh/kudurru/internal/upstream"
	"github.com/robertherbaugh/kudurru/internal/version"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
	"github.com/robertherbaugh/kudurru/internal/zoneview"

	"k8s.io/client-go/rest"
)

func newServeCmd(configPath *string) *cobra.Command {
	var licenseKey string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the DNS server",
		Long: `Run the kudurru node: the store (sqlite or kube), first-boot bootstrap,
file-managed objects, zonecache, the query pipeline (ingress, authoritative,
cache, upstream) on every configured transport, the Lease-elected controller,
the ops and metrics listeners. The API and UI land in milestone M3.`,
		RunE: func(c *cobra.Command, _ []string) error {
			cfg, err := loadConfigWithSections(c, *configPath, func(base *config.Config) (map[string]any, error) {
				backend, err := settingsBackend(base)
				if err != nil {
					return nil, err
				}
				if backend == nil {
					return nil, nil
				}
				ctx, cancel := context.WithTimeout(c.Context(), 15*time.Second)
				defer cancel()
				d, _, err := settings.ReadStartup(ctx, backend)
				return d.Sections(), err
			})
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(c.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return runServe(ctx, cfg, licenseKey)
		},
	}
	cmd.Flags().StringVar(&licenseKey, "license-key", "", "license key (or KUDURRU_LICENSE_KEY, KUDURRU_LICENSE_KEY_FILE, <node.dataDir>/license.key); without one kudurru runs with the community license")
	return cmd
}

// logLevel is the level of the serve logger; log.level reloads set it.
var logLevel = new(slog.LevelVar)

func newLogger(cfg config.Log) *slog.Logger { return newLoggerAt(cfg, new(slog.LevelVar)) }

// newLoggerAt builds the logger on level, set from cfg.Level.
func newLoggerAt(cfg config.Log, level *slog.LevelVar) *slog.Logger {
	level.Set(parseLevel(cfg.Level))
	opts := &slog.HandlerOptions{Level: level}
	if cfg.Format == "text" {
		return slog.New(slog.NewTextHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, opts))
}

// openStore opens the configured driver (dev-docs/design-record.md §4.3, §4.4).
// The returned snapshot store is non-nil only for the kube driver.
// openStore opens the configured driver (dev-docs/design-record.md §4.3, §4.4)
// with the public and internal kudurru.io kinds. The rest config is non-nil
// only for the kube driver.
func openStore(ctx context.Context, cfg *config.Config, v *validator.Validator, reg prometheus.Registerer, logger *slog.Logger) (store.Store, *localdb.SnapshotStore, *rest.Config, error) {
	if err := prepareStoragePaths(cfg); err != nil {
		return nil, nil, nil, err
	}
	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		return nil, nil, nil, err
	}
	var st store.Store
	var restCfg *rest.Config
	switch cfg.Store.Driver {
	case "sqlite":
		s, err := sqlite.Open(filepath.Join(cfg.Node.DataDir, sqlite.FileName), sqlite.Options{
			Validator: v, Scheme: scheme, InternalKinds: privateStoreKinds(),
			Identity: cfg.Node.Name, Logger: logger.With("component", "store"), Metrics: sqlite.NewMetrics(reg),
			ValidateReplicaEntry: runtimeowners.ValidateEntry, ValidateReplicaImage: runtimeowners.ValidateImage,
		})
		switch {
		case errors.Is(err, sqlite.ErrSchemaTooNew):
			return nil, nil, nil, fmt.Errorf("state.db was written by a newer kudurru: %w", err)
		case errors.Is(err, sqlite.ErrLocked):
			return nil, nil, nil, fmt.Errorf("another kudurru process owns state.db: %w", err)
		case err != nil:
			return nil, nil, nil, err
		}
		// Seed explicit security and inventory manifests under the same persisted
		// writer fence as commands. An enrolled follower receives it only from
		// its complete primary image; missing or malformed history is not reset.
		if s.Authority().Writable() {
			if err := s.Command(ctx, s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
				if err := runtimeowners.Initialize(ctx, tx, time.Now().UTC()); err != nil {
					return err
				}
				return initializeServingSettings(ctx, tx, cfg)
			}); err != nil {
				_ = s.Close()
				return nil, nil, nil, fmt.Errorf("initialize runtime owners: %w", err)
			}
		}
		st = s
	case "kube":
		restCfg, err = ctrlconfig.GetConfig()
		if err != nil {
			return nil, nil, nil, fmt.Errorf("store.driver kube: %w", err)
		}
		s, err := kube.New(restCfg, kube.Options{
			Scheme: scheme, InternalKinds: privateStoreKinds(),
			Identity: cfg.Node.Name, ExpectedClusterID: cfg.Store.Kube.ExpectedClusterID, StartupTimeout: cfg.Store.Kube.StartupTimeout.Std(), Logger: logger.With("component", "store"),
		})
		if err != nil {
			return nil, nil, nil, err
		}
		checkCtx, cancel := context.WithTimeout(ctx, kubeStartupTimeout(cfg))
		identityErr := s.CheckClusterIdentity(checkCtx)
		cancel()
		if ctx.Err() != nil || identityErr != nil && !kube.IsUnavailable(identityErr) {
			_ = s.Close()
			if ctx.Err() != nil {
				return nil, nil, nil, ctx.Err()
			}
			return nil, nil, nil, identityErr
		}
		restCfg = s.RESTConfig()
		st = s
	default:
		return nil, nil, nil, fmt.Errorf("store.driver %q is not supported", cfg.Store.Driver)
	}
	db, err := localdb.OpenSnapshotStore(ctx, filepath.Join(cfg.LocalDataDir(), localdb.FileName))
	if err != nil {
		_ = st.Close()
		if errors.Is(err, localdb.ErrSchemaTooNew) {
			return nil, nil, nil, fmt.Errorf("local.db was written by a newer kudurru: %w", err)
		}
		return nil, nil, nil, err
	}
	return st, db, restCfg, nil
}

func runServe(ctx context.Context, cfg *config.Config, licenseKey string) error {
	logger := newLoggerAt(cfg.Log, logLevel)
	slog.SetDefault(logger)
	// Settings hot reload (D25): every consumer of a cluster-scope key that
	// is not restartRequired subscribes to reload (reload.go).
	info := version.Get()
	logger.Info("kudurru starting", "version", info.Version, "commit", info.Commit, "node", cfg.Node.Name, "config", cfg.Source, "store", cfg.Store.Driver)

	if err := prepareStoragePaths(cfg); err != nil {
		return err
	}
	cfg, dhcpSetup, dhcpServer, dhcpSetupError, closeDHCP, err := prepareDHCP(cfg, logger)
	if err != nil {
		return err
	}
	defer closeDHCP()
	reload := config.NewReloadable(cfg)
	if dhcpServer != nil {
		logger.Info("DHCPv4 listeners ready", "scopes", dhcpServer.ScopeNames())
	}
	leaseDNS := dhcpLocalRecords(dhcpServer)

	// Metrics registry: build info, Go runtime, process, DNS.
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	build := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "kudurru_build_info", Help: "Build information."}, []string{"version", "revision", "goversion"})
	build.WithLabelValues(info.Version, info.Commit, info.GoVersion).Set(1)
	reg.MustRegister(build)
	dnsMetrics := metrics.NewDNS(reg)

	// License (docs/architecture/licensing.md): verified offline, reloaded
	// from its sources every minute. It only ever narrows the management
	// plane (node limit, ee/ features); DNS answering never depends on it.
	lic, err := newLicenseManager()
	if err != nil {
		return err
	}
	licSources := license.DefaultSources(licenseKey, cfg.Node.DataDir, os.Getenv)
	if err := lic.Load(licSources); err != nil {
		logger.Warn("license key", "error", err)
	}
	if cfg.Branding != (config.Branding{}) && !lic.Allows(license.FeatureBranding) {
		logger.Warn("custom branding is inactive: an Enterprise license including branding is required; using built-in presentation")
	}
	reg.MustRegister(license.NewCollector(lic))

	checker := health.New(map[string]any{"version": info.Version, "node": cfg.Node.Name})

	// Store: one validator per process, shared by the driver and the loaders.
	v, err := validator.New()
	if err != nil {
		return err
	}
	st, snapDB, restCfg, err := openStore(ctx, cfg, v, reg, logger)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	defer func() { _ = snapDB.Close() }()
	backend, err := settingsBackend(cfg)
	if err != nil {
		return err
	}
	var settingsManager *settings.Manager
	if backend != nil {
		settingsManager = &settings.Manager{Backend: backend, Reload: reload, Environ: os.Environ(), Logger: logger}
		initCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		err = settingsManager.Initialize(initCtx)
		cancel()
		if err != nil {
			return fmt.Errorf("settings ownership: %w", err)
		}
		cfg = reload.Current()
	}
	var dhcpKube *dhcpkube.Runtime
	if cfg.Store.Driver == "kube" && cfg.DHCP.Enabled {
		if dhcpKube, err = openDHCPKube(cfg, st, restCfg, logger); err != nil {
			return fmt.Errorf("dhcp: %w", err)
		}
		leaseDNS.SetSource(dhcpKube.Stores)
	}
	// Retain the concrete authority before any management facade is installed.
	// Followers receive bootstrap state only in a validated complete replica.
	sql, _ := st.(*sqlite.Store)
	bootstrapWritable := sql == nil || sql.Authority().Writable()
	var complete *servingRuntime
	var zoneReads *servingbuild.Diagnostics
	var capturedZoneReads api.ZoneReadSource

	// Use the release namespace for seeds, policy fallback and API writes.
	// Preserve the namespace of a legacy default policy on upgrades.
	releaseNamespace := bootstrap.DefaultNamespace
	namespace := bootstrap.DefaultNamespace
	if cfg.Store.Driver == "kube" {
		releaseNamespace = kube.PodNamespace()
		initCtx, cancel := context.WithTimeout(ctx, kubeStartupTimeout(cfg))
		namespace, err = bootstrap.ResolveNamespace(initCtx, st, releaseNamespace)
		cancel()
		if err != nil {
			if ctx.Err() != nil || !kube.IsUnavailable(err) {
				return err
			}
			// Select the same policy fallback as the last serving snapshot.
			// Never ignore malformed SQLite data or reuse another cluster's
			// namespace without an independently trusted matching identity.
			namespace = releaseNamespace
			saved, loadErr := snapDB.Load(ctx)
			if loadErr != nil && !errors.Is(loadErr, localdb.ErrNoSnapshot) {
				return loadErr
			}
			if loadErr == nil && localdb.CheckClusterIdentity(cfg.Store.Kube.ExpectedClusterID, st.Info().ClusterID, saved.ClusterID) == nil {
				namespace = bootstrap.NamespaceFromObjects(saved.Objects, releaseNamespace)
			}
			logger.Warn("policy namespace read deferred; using admitted startup namespace", "namespace", namespace, "error", err)
		}
		if namespace != releaseNamespace {
			logger.Warn("keeping legacy default policy namespace", "namespace", namespace, "releaseNamespace", releaseNamespace)
		}
	}
	// An incomplete offline IAM cutover must not seed accounts or admit remote
	// identities. This gate performs no work on the DNS path. Recheck every IAM
	// mutation independently of the HTTP request's earlier admission result.
	var identityGate *iamgate.Gate
	if restCfg != nil {
		migration, err := newKubeIAMMigrator(restCfg, cfg, releaseNamespace)
		if err != nil {
			return err
		}
		identityGate = iamgate.New(migration)
		initCtx, cancel := context.WithTimeout(ctx, kubeStartupTimeout(cfg))
		_, status, err := identityGate.Capture(initCtx)
		cancel()
		if err != nil {
			logger.Warn("remote identity admission disabled; inspect kudurru iam migrate status", "state", status.State, "releaseNamespace", releaseNamespace)
		}
		st = &iamgate.Store{Store: st, Gate: identityGate}
	}

	// Audit in Kubernetes (§11.4, #10): the kube driver keeps no audit rows,
	// so store writes and API actions are recorded in the node's audit
	// spool (audit-spool.db) and as "log":"audit" lines. The wrapper goes
	// on before the first writer (bootstrap) takes the store.
	var auditSpool *spool.Spool
	if cfg.Store.Driver == "kube" {
		auditSpool, err = spool.Open(ctx, spool.Options{Path: filepath.Join(cfg.Node.DataDir, spool.FileName), Node: cfg.Node.Name, Registerer: reg})
		if err != nil {
			if errors.Is(err, spool.ErrSchemaTooNew) {
				return fmt.Errorf("audit-spool.db was written by a newer kudurru: %w", err)
			}
			return err
		}
		defer func() { _ = auditSpool.Close() }()
		scheme, err := runtimekinds.NewScheme()
		if err != nil {
			return err
		}
		st = audit.WrapStore(st, audit.Multi{audit.Log{Logger: logger}, auditSpool},
			audit.WrapOptions{Node: cfg.Node.Name, Scheme: scheme, Logger: logger})
	}

	// First boot: root forward zone, default client group and policy (§6.4).
	continueSeed := false
	seedDefaults := func(ctx context.Context) error {
		if cfg.Store.Driver == "kube" {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, kubeStartupTimeout(cfg))
			defer cancel()
		}
		// nosemgrep: invalid-usage-of-modified-variable -- Seed returns a Result value, filled on error too
		seed, err := bootstrap.Seed(ctx, st, bootstrap.Options{Namespace: namespace, Validator: v, Logger: logger, StarterBlockList: bootstrap.EmbeddedStarterBlockList, ContinueFirstBoot: continueSeed})
		if err != nil {
			continueSeed = continueSeed || seed.FirstBoot
			return fmt.Errorf("bootstrap: %w", err)
		}
		if seed.FirstBoot {
			logger.Info("first boot: seeded defaults", "created", len(seed.Created))
		}
		return nil
	}
	seedPending := false
	if err := func() error {
		if !bootstrapWritable {
			return nil
		}
		return seedDefaults(ctx)
	}(); err != nil {
		if ctx.Err() != nil || cfg.Store.Driver != "kube" || !kube.IsUnavailable(err) {
			return err
		}
		seedPending = true
		logger.Warn("bootstrap deferred until the apiserver is available", "error", err)
	}

	// File-managed objects (§6.2): inline objects and objectsDir.
	inlineSource := "kudurru.yaml"
	if cfg.Source != "" {
		inlineSource = filepath.Base(cfg.Source)
	}
	inlineFile := cfg.Source
	if slices.Contains(cfg.EnvLocked, "objects") {
		inlineFile = "" // the process environment retains precedence over the file
	}
	loader, err := objects.New(objects.Options{
		Store: st, Validator: v, Logger: logger, Registerer: reg,
		Inline: cfg.Objects, InlineFile: inlineFile, InlineSource: inlineSource, Dir: cfg.ObjectsDir,
	})
	if err != nil {
		return err
	}
	loadCtx := ctx
	loadCancel := func() {}
	if cfg.Store.Driver == "kube" {
		loadCtx, loadCancel = context.WithTimeout(ctx, kubeStartupTimeout(cfg))
	}
	if bootstrapWritable {
		if report := loader.Load(loadCtx); len(report.Diagnoses) > 0 {
			for _, d := range report.Diagnoses {
				logger.Warn("object file diagnosis", "diagnosis", d)
			}
		}
	}
	loadCancel()
	checker.Register("objects", func() (bool, []string) {
		if sql != nil && !sql.Authority().Writable() {
			// A follower intentionally never loads local desired objects. Its
			// complete replicated image is checked by the serving readiness gate.
			return true, nil
		}
		return loader.Ready()
	})

	// zonecache: store watches → serving snapshots, local.db cold start (§4.4).
	var zcLocal *localdb.SnapshotStore
	if cfg.Store.Driver == "kube" {
		zcLocal = snapDB
	}
	zc, err := zonecache.New(st, zonecache.Options{
		LocalDB: zcLocal, ExpectedClusterID: cfg.Store.Kube.ExpectedClusterID, StartupTimeout: cfg.Store.Kube.StartupTimeout.Std(), MaxAge: cfg.Store.Kube.Snapshot.MaxAge.Std(),
		Registerer: reg, Logger: logger,
	})
	if err != nil {
		return err
	}
	if sql == nil {
		if err := zc.Register(zoneview.Builder(logger)); err != nil {
			return err
		}
	}
	// Blocked services and safe-search catalog (D60): embedded, replaced
	// at runtime from blocking.catalogue.url, plus the opt-in AdGuard
	// services import (blocking.catalogue.adguardServicesURL).
	catalogs := catalogue.NewHolder(nil)
	clientInventory := inventory.New(nil)
	if err := zc.Register(policy.Builder(policy.Options{DefaultNamespace: namespace, Services: catalogs, Inventory: true, Hostnames: leaseDNS}, logger)); err != nil {
		return err
	}
	checker.Register("zonecache", zc.Ready)

	// Blocklists (§7.3, D15): the leader fetches, every node pulls bodies by
	// digest over the internal listener (D47) and compiles locally.
	lists, err := listfetcher.OpenDiskCache(cfg.BlockingListsDir(), logger)
	if err != nil {
		return err
	}
	listObjs := listfetcher.NewZoneCacheObjects(zc)
	downloader := listfetcher.NewDownloader(lists, listfetcher.DownloadOptions{AllowLocalAddresses: cfg.Blocking.AllowLocalListURLs, HTTPSOnly: cfg.Blocking.HTTPSOnly})
	initStrategy, err := listfetcher.ParseInitStrategy(cfg.Blocking.InitStrategy)
	if err != nil {
		return err
	}
	fetcher, err := listfetcher.New(listfetcher.Options{
		Store: st, Objects: listObjs, Cache: lists, Downloader: downloader,
		RefreshInterval: cfg.Blocking.RefreshInterval.Std(), Registerer: reg, Logger: logger,
	})
	if err != nil {
		return err
	}
	var membership cluster.Membership = cluster.Self{Name: cfg.Node.Name}
	registerHealthDetails(checker, st.Elector(cluster.ElectionController), cfg.Node.Name, zc.Snapshot)
	if cfg.Store.Driver == "kube" {
		headless := os.Getenv("KUDURRU_HEADLESS_SERVICE")
		_, port, _ := net.SplitHostPort(cfg.Listeners.Internal.Address)
		if port == "" {
			port = "8082"
		}
		byName := func(_ context.Context, pod string) (string, error) {
			host := pod
			if headless != "" {
				host = pod + "." + headless
			}
			return net.JoinHostPort(host, port), nil
		}
		resolve, source, err := leaderResolver(restCfg, headless, port, byName)
		if err != nil {
			return err
		}
		membership = &cluster.ElectorMembership{
			SelfName: cfg.Node.Name,
			Elector:  st.Elector(cluster.ElectionController),
			Resolve:  resolve,
			Source:   source,
			CacheTTL: 30 * time.Second,
		}
	}
	encKeys, err := keys.Load(keys.Options{Driver: cfg.Store.Driver, DataDir: cfg.Node.DataDir})
	if err != nil {
		return err
	}
	encKey, encPrev := encKeys.Current, encKeys.Previous
	if encKeys.Generated {
		logger.Info("generated the encryption key", "path", encKeys.Path)
	}
	// Every replica receives external zones independently. The cluster key
	// binds persisted copies even during an apiserver outage; rotating it
	// requires fresh transfers rather than trusting an unrelated local copy.
	keyring, err := dnsserver.KeyringFromConfig(cfg.TSIGKeys)
	if err != nil {
		return err
	}
	secondaryCluster := sha256.Sum256(append([]byte("kudurru/secondary/cluster/v1:"), encKey...))
	secondaries, err := secondary.New(secondary.Options{
		Cache: zc, Blobs: snapDB, ClusterID: fmt.Sprintf("%x", secondaryCluster), Keyring: keyring,
		Receiver: &secondary.TransferClient{Keyring: keyring}, Logger: logger,
	})
	if err != nil {
		return err
	}
	if sql == nil {
		reg.MustRegister(zoneview.NewCollector(zc, secondaries))
	}
	reg.MustRegister(secondary.NewCollector(secondaries))
	stubClient := &stub.Client{}
	stubCluster := sha256.Sum256(append([]byte("kudurru/stub/cluster/v1:"), encKey...))
	stubs, err := stub.New(stub.Options{
		Cache: zc, Blobs: snapDB, ClusterID: fmt.Sprintf("%x", stubCluster), Discoverer: stubClient, Logger: logger,
	})
	if err != nil {
		return err
	}
	reg.MustRegister(stub.NewCollector(stubs))
	var feeds *rpz.Manager
	if sql != nil {
		rpzCluster := sha256.Sum256(append([]byte("kudurru/rpz/cluster/v1:"), encKey...))
		feeds, err = rpz.New(rpz.Options{Cache: zc, Blobs: snapDB, ClusterID: fmt.Sprintf("%x", rpzCluster), Keyring: keyring, Receiver: &secondary.TransferClient{Keyring: keyring}, Logger: logger})
		if err != nil {
			return err
		}
	}
	notifyReceiver := secondary.NewNotifyReceiver(secondaries, reg)
	var (
		internalClient *internalapi.Client
		internalSrv    *internalapi.Server
		internalCreds  *internalapi.Credentials
		listResolver   *listfetcher.Resolver
		qlTail         *querylog.Tail
		qlLogs         *querylogRuntime
		queryUpstreams func() []upstream.ServerStatus
	)
	if encKey != nil || cfg.Listeners.Internal.TokenFile != "" {
		creds, err := internalapi.NewCredentials(internalapi.CredentialOptions{
			Key: encKey, PreviousKey: encPrev, NodeName: cfg.Node.Name,
			TokenFile: cfg.Listeners.Internal.TokenFile,
			CertFile:  cfg.Listeners.Internal.TLS.CertFile, KeyFile: cfg.Listeners.Internal.TLS.KeyFile,
			CAFile: cfg.Listeners.Internal.TLS.CAFile, ServerName: cfg.Listeners.Internal.TLS.ServerName,
		})
		if err != nil {
			return err
		}
		internalCreds = creds
		internalClient = internalapi.NewClient(creds, internalapi.ClientOptions{})
		internalSrv, err = internalapi.NewServer(internalapi.ServerOptions{
			Event: func(e internalapi.Event) bool { return secondaries.Trigger(e.Identity) },
			Querylog: func(ctx context.Context, q internalapi.QuerylogRequest) (*internalapi.QuerylogResponse, error) {
				out, err := internalapi.ReadLocalQuerylog(ctx, qlLogs.reader(), q)
				if err == nil && q.Kind == "upstreams" && queryUpstreams != nil {
					out.Servers = queryUpstreams()
				}
				return out, err
			},
			QueryTail: func() *querylog.Tail { return qlTail },
			Address:   cfg.Listeners.Internal.Address, Credentials: creds, Lists: lists, Catalogue: catalogueBody(catalogs), CatalogueImport: catalogueImportBody(catalogs),
			Status: func(ctx context.Context) internalapi.Status {
				leader, _ := membership.Leader(ctx)
				var ls []internalapi.ListStatus
				if listResolver != nil {
					ls = listResolver.Lists()
				}
				ready, unready := checker.Ready()
				return internalapi.Status{
					Node: cfg.Node.Name, Version: info.Version, Leader: leader.Name, State: zc.State().String(), Lists: ls, Ready: &ready, Unready: unready,
					Snapshot: zc.Snapshot().SnapshotAge(time.Now()),
				}
			},
			Health: func(ctx context.Context) (internalapi.Health, error) {
				return internalapi.LocalHealth(ctx, cfg.Node.Name, checker.Handler())
			},
			// Zones and serials for the edge supervisors (§3.6 step 5).
			Zones: func(ctx context.Context) internalapi.ZoneList {
				zl := internalapi.ZoneList{Node: cfg.Node.Name}
				if zoneReads != nil {
					for _, z := range capturedAuthoritativeZones(ctx, zoneReads) {
						zl.Zones = append(zl.Zones, internalapi.Zone{Name: z.Apex, Serial: z.Serial, Type: string(z.Type)})
					}
					return zl
				}
				for _, z := range zoneview.Served(zc, secondaries) {
					zl.Zones = append(zl.Zones, internalapi.Zone{Name: z.Name(), Serial: z.Serial(), Type: string(z.Type())})
				}
				return zl
			},
			Registerer: reg, Logger: logger,
		})
		if err != nil {
			return err
		}
	} else if cfg.Store.Driver == "kube" {
		return errors.New("KUDURRU_ENCRYPTION_KEY (or listeners.internal.tokenFile) is required for the internal listener in Kubernetes")
	}
	notifyFanout := internalapi.NewEventFanout(membership, internalClient, reg, logger)
	notifyReceiver.Accepted = func(identity string) {
		notifyFanout.Enqueue(internalapi.Event{Type: "secondary-notify", Identity: identity})
	}
	// A dnsdist next to this node (sidecar, compose single-edge, HA nodes)
	// caches blocked and unblocked answers up to their TTL: flush its
	// resolver pool on every compiled list snapshot and on blocking toggles
	// (dev-docs/milestones/m3/edge.md). Without a local edge the flusher is a no-op.
	edgeFlush := dnsdist.NewLocalCacheFlusher("", "", logger.With("component", "edgeflush"))
	defer edgeFlush.Stop()
	listResolver, err = listfetcher.NewResolver(listfetcher.ResolverOptions{
		Objects: listObjs, Cache: lists, Sink: flushingListSink{next: zc, flush: edgeFlush}, Membership: membership, Client: internalClient,
		Downloader: downloader, RefreshInterval: cfg.Blocking.RefreshInterval.Std(),
		InitStrategy: initStrategy, AllowDegraded: cfg.Blocking.AllowDegraded, Extra: catalogs,
		Registerer: reg, Logger: logger,
	})
	if err != nil {
		return err
	}
	checker.Register("blocklists", listResolver.Ready)
	catRefreshers, err := newCatalogueRefreshers(cfg, catalogs, membership, internalClient, logger)
	if err != nil {
		return err
	}

	// Query log (§11.2): privacy once at ingest, live tail, sqlite or slog sink.
	listedDomains := func() int64 {
		if sn := zc.Snapshot(); sn != nil {
			return domainsOnLists(sn.Objects.Lists())
		}
		return 0
	}
	qlLogs, err = newQuerylogRuntime(ctx, cfg, reg, logger, listedDomains)
	if err != nil {
		return err
	}
	defer func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		if err := qlLogs.close(closeCtx); err != nil {
			logger.Warn("query log flush incomplete", "err", err, "dropped", qlLogs.async.Dropped())
		}
	}()
	qlTail = qlLogs.tail
	qlAsync := qlLogs.async
	checker.Register("querylog", qlLogs.ready)

	// Postprocess wraps all answering stages; DNSSEC validates raw forwarded
	// answers before the cache stores them. The log observer sees the result.
	// One recursive resolver for every recursive group, kept across reloads
	// so its delegation and key caches survive settings changes.
	var rec *recursor.Resolver
	recMetrics := recursor.NewMetrics(reg, func() int {
		if rec == nil {
			return 0
		}
		return rec.CacheLen()
	})
	rec, err = recursor.New(upstream.RecursionFrom(cfg.Upstreams.Recursion), recursor.Options{Logger: logger.With("component", "recursor"), Metrics: recMetrics})
	if err != nil {
		return err
	}
	upOpts := upstream.Options{Logger: logger, Metrics: upstream.NewMetrics(reg), Recursor: rec, RecursorMetrics: recMetrics}
	upSet, err := upstream.New(upstream.ConfigFrom(cfg.Upstreams), upOpts)
	if err != nil {
		return err
	}
	fwd := upstreamstage.NewStage(upSet)
	validation, err := cfg.DNSSEC.Validator(nil)
	if err != nil {
		return err
	}
	if err := fwd.SetValidation(validation, cfg.DNSSEC.Enabled); err != nil {
		return err
	}
	queryUpstreams = func() []upstream.ServerStatus { return fwd.Set().Status() }
	hostname, _ := os.Hostname()

	// Ingress rate limiting (§7.1 step 2).
	// The limiter always exists so ratelimit.enabled can change at runtime.
	rlc, err := ingress.RateLimitFromConfig(cfg.RateLimit)
	if err != nil {
		return err
	}
	limiter, err := ingress.NewRateLimiter(ingress.RateLimitConfig{})
	if err != nil {
		return err
	}
	if err := limiter.Reconfigure(rlc); err != nil {
		return err
	}
	limiter.SetHook(func(v ingress.Verdict) {
		if v == ingress.VerdictDrop {
			dnsMetrics.Dropped.WithLabelValues(ingress.DropReason).Inc()
		}
	})
	reg.MustRegister(limiter.Collector(), limiter.SlipCollector())

	// Policies and blocking (§7.1 steps 3, 4, 7).
	polSrc := policy.Source{Cache: zc}
	bopts := blockingstage.OptionsFromConfig(cfg)
	bopts.Index, bopts.Registerer, bopts.Catalogue = polSrc, reg, catalogs
	bopts.Context = func(req *pipeline.Request) blocking.Context {
		q := req.Question()
		return blocking.Context{Name: q.Name, Type: q.Qtype, Class: q.Qclass, ClientIP: req.Client.Addr.Addr(), ClientID: req.Client.ClientID, ClientNames: req.Client.Names, Tags: req.Client.Tags}
	}
	var rewriteTarget pipeline.Next
	bopts.ResolveTarget = func(req *pipeline.Request) (*pipeline.Response, error) {
		return rewriteTarget(req)
	}
	bs, err := blockingstage.New(bopts)
	if err != nil {
		return err
	}
	if sql == nil {
		checker.Register("blocking", bs.Ready)
	}
	bs.Toggle().SetOnChange(edgeFlush.BlockingChanged)

	// Block page (docs/deploy/blocking-page.md): the renderer always exists
	// for the API preview; its listener runs when blockPage.enabled.
	// Unblock requests (the block page's form, /unblockrequests): the book
	// indexes them on every node; the leader expires and prunes them.
	unblockBook, err := newUnblockBook(st, reload, namespace, cfg.Node.Name, encKey, logger)
	if err != nil {
		return err
	}
	var brandCache branding.PageCache
	var brandSource []func(context.Context) (*blockpage.Branding, error)
	if sql != nil {
		brandSource = append(brandSource, func(ctx context.Context) (*blockpage.Branding, error) {
			return complete.captureBlockPageBranding(ctx, &brandCache)
		})
	}
	blockPage, err := newBlockPage(cfg, reload, polSrc, qlTail, qlLogs.reader(), qlAsync, qlLogs.reader, unblockBook, lic, reg, logger, brandSource...)
	if err != nil {
		return err
	}

	var zoneSource authoritative.Source
	if sql == nil {
		zoneSource = zoneview.Source{Cache: zc, Secondary: secondaries, Stub: stubs}
	}
	auth := authoritative.New(zoneSource)
	auth.Metrics = authoritative.NewMetrics(reg)
	auth.LocalRecords = leaseDNS
	auth.Notify = notifyReceiver.Handle
	auth.StubQuery = stubClient.Exchange

	responseSettings, err := cfg.Response.Compile()
	if err != nil {
		return err
	}
	post := &postprocess.Stage{}
	post.SetSettings(responseSettings)
	idStage := identify.New(polSrc, nil)
	idStage.Inventory = clientInventory
	idStage.Hostnames = leaseDNS
	controlView, err := control.ViewFromConfig(cfg.Control)
	if err != nil {
		return err
	}
	controlStage := control.New(control.Options{})
	if err := controlStage.Replace(controlView); err != nil {
		return err
	}
	stages := []pipeline.Stage{
		&ingress.Stage{NodeID: cfg.Node.Name, Hostname: hostname, Version: info.Version, Limiter: limiter, GroupExempt: idStage.RateLimitExempt, Lameduck: checker.IsLameduck},
		idStage,
		controlStage,
		post,
		auth,
		bs,
		&rewrite.Stage{GuardDependency: controlStage.Serve},
	}
	// The cache stage always exists; cache.maxEntries 0 makes it pass
	// queries through, so the key can change at runtime.
	rc := cache.New(cache.OptionsFrom(cfg.Cache, cache.NewMetrics(reg)))
	cs := cachestage.New(rc, cachestage.SettingsFrom(cfg.Cache), fwd)
	cs.SetDisabled(cfg.Cache.MaxEntries == 0)
	stages = append(stages, cs, dnssecstage.New(fwd), fwd)
	chain, err := pipeline.New(stages...)
	if err != nil {
		return err
	}
	// Rewrite dependencies keep the captured client/settings but enter normal
	// authority and recursive policy dispatch for their own target name. This
	// prevents CNAME rewrites from bypassing configured child-zone coverage.
	dependencies, err := pipeline.New(controlStage, auth, bs, &rewrite.Stage{GuardDependency: controlStage.Serve}, cs, dnssecstage.New(fwd), fwd)
	if err != nil {
		return err
	}
	rewriteTarget = dependencies.Serve
	loggingObserver := &logstage.Observer{Node: cfg.Node.Name, Collector: qlAsync, Metrics: dnsMetrics}
	chain.Observe(loggingObserver)
	logger.Info("pipeline ready", "stages", chain.Stages(), "upstreamGroups", upSet.Groups())

	// TSIG keys for transfers and NOTIFY (RFC 8945).
	tsigFailures := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "kudurru_dns_tsig_failures_total", Help: "Requests rejected for their TSIG (RFC 8945), by error."}, []string{"error"})
	reg.MustRegister(tsigFailures)

	// DNS listeners: udp/tcp/dot/doh/doq/doh3 with the shared certificate
	// store and the authenticated PROXYv2 policy (D45, D56, D67).
	proxyPolicy, err := dnsserver.PolicyFromConfig(cfg.Listeners)
	if err != nil {
		return err
	}
	if proxyPolicy != nil {
		proxyPolicy.SetDropHook(func(r proxyproto.Reason) { dnsMetrics.Dropped.WithLabelValues(string(r)).Inc() })
	}
	registry, err := dnsserver.NewRegistry(dnsserver.Options{
		Handler:       &dnsserver.Handler{Chain: chain, Logger: logger},
		Logger:        logger,
		Proxy:         proxyPolicy,
		TSIG:          keyring,
		OnTSIGFailure: func(reason string) { tsigFailures.WithLabelValues(reason).Inc() },
		Sockets:       cfg.Listeners.Sockets,
		Limits:        dnsserver.LimitsFromConfig(cfg.Listeners),
	})
	if err != nil {
		return err
	}
	if err := registry.Add(dnsserver.SpecsFromConfig(cfg.Listeners)...); err != nil {
		return err
	}
	if err := registry.Bind(); err != nil { // fail fast on port conflicts
		return err
	}
	defer func() {
		for _, listener := range registry.Listeners() {
			_ = listener.Close()
		}
	}()
	checker.Register("dns", registry.Ready)
	if cfg.BlockPage.Enabled {
		if err := blockPage.Listen(); err != nil { // fail fast on port conflicts
			return err
		}
	}

	// SQLite's lifecycle driver owns durable NOTIFY. The legacy notifier is
	// retained only for the Kubernetes serving path.
	var nt *notifier.Notifier
	var manualNotify api.ZoneNotifier
	if sql == nil {
		var notifySrc netip.Addr
		if cfg.Notify.Source != "" {
			notifySrc = netip.MustParseAddr(cfg.Notify.Source)
		}
		nt, err = notifier.New(notifier.Options{Source: zoneview.Source{Cache: zc}, Keyring: keyring, SourceAddr: notifySrc, Registerer: reg, Logger: logger})
		if err != nil {
			return err
		}
		zc.OnPublish(func(*zonecache.Snapshot) { nt.Trigger() })
		manualNotify = api.NotifyTrigger(nt.Trigger)
	}

	// Response cache persistence across restarts (local.db blob).
	var cachePersister *persist.Persister
	if cfg.Cache.MaxEntries > 0 && snapDB != nil {
		if cachePersister, err = persist.New(snapDB, persist.Options{}); err != nil {
			return err
		}
		if pst, err := cachePersister.Restore(ctx, rc); err != nil {
			logger.Warn("cache dump not restored", "err", err)
		} else if pst.Entries > 0 {
			logger.Info("cache restored", "entries", pst.Entries, "age", time.Since(pst.WrittenAt).Round(time.Second))
		}
	}

	// Identity (§8): users, tokens, sessions, OIDC, Kubernetes identities,
	// the RBAC engine and the admin socket.
	stackOpts := identityOptions(cfg, st, encKeys, logger)
	crossOrigin := http.NewCrossOriginProtection()
	stackOpts.CrossOrigin = crossOrigin
	stackOpts.DisableBootstrap = !bootstrapWritable
	stackOpts.DisableProviderDiscovery = sql != nil
	if identityGate != nil {
		stackOpts.TokenMigration = iamgate.ResolveToken
		stackOpts.InitializationAdmission = func(ctx context.Context) error {
			_, _, err := identityGate.Capture(ctx)
			return err
		}
	}
	stackOpts.Kube = restCfg
	stackOpts.OIDCAllowed = func() bool { return lic.Allows(license.FeatureOIDC) }
	stackOpts.RBACAllowed = func() bool { return lic.Allows(license.FeatureRBAC) }
	if cfg.Store.Driver == "kube" {
		stackOpts.Namespace = releaseNamespace
		stackOpts.RetryUnavailable = func(err error) bool {
			return kube.IsUnavailable(err) || errors.Is(err, iamgate.ErrClosed)
		}
		stackOpts.InitializationTimeout = kubeStartupTimeout(cfg)
	}
	if internalCreds != nil {
		stackOpts.InternalToken = internalCreds.ValidToken
	}
	ids, err := stack.New(ctx, stackOpts)
	if err != nil {
		return fmt.Errorf("auth: %w", err)
	}
	authenticate := identityAuthentication(ids, identityGate)
	authorizer, roles := ids.Authorizer(), ids.Roles
	var authHandler http.Handler
	// Complete SQLite settings use the fenced canonical command path; legacy
	// ownership actions must not write a separate settings file beside it.
	if complete != nil {
		settingsManager = nil
	}
	var dnsQueryCapture pipeline.Capture
	var canonicalSettings api.CanonicalSettings
	var brandingReads api.BrandingReadSource
	var blockPagePreview api.BlockPagePreviewer = blockPage
	var nodeConfigCommand api.NodeConfigCommand
	var cacheFlushCommand api.CacheFlushCommand
	var objectCommand api.ObjectCommand
	var objectReads api.ObjectReadSource
	var infoMessageReads api.InfoMessageReadSource
	var sourceBodyReads api.SourceBodyReadSource
	var policyListReads api.PolicyListReadSource
	var blockingReads api.BlockingReadSource
	var blockingCommand api.BlockingCommand
	var upstreamReads api.UpstreamReadSource
	var migrationReads api.MigrationReadSource
	var migrationCommand api.MigrationCommand
	var zoneCommand api.ZoneCommand
	var unblockCommand unblock.Command
	var domainCommand api.DomainCommand
	var zoneNotifyCommand api.ZoneNotifyCommand
	catalogSource := catalogueAPI(catalogs, catRefreshers)
	if sql == nil {
		logger.Warn("background integration execution is unavailable for the Kubernetes store; atomic source publication is required")
		authMux := http.NewServeMux()
		ids.Register(authMux)
		registerUnavailableTOTP(authMux)
		registerUnavailableLocalUsers(authMux)
		authHandler = authMux
	}
	if sql != nil {
		authKeys, err := newAuthKeyOwner(ctx, encKeys)
		if err != nil {
			return err
		}
		rings, err := servingbuild.NewKeyringOwner(keyring)
		if err != nil {
			return err
		}
		defer func() { _ = rings.Close(context.Background()) }()
		complete, err = newServingRuntime(ctx, servingRuntimeOptions{
			Store: sql, Config: cfg, Validator: v, License: lic, AuthKeys: authKeys,
			Keyrings: rings, Catalog: catalogs, Transferred: servingbuild.NewTransferredRuntime(secondaries, stubs, feeds),
			Lists: lists, Namespace: namespace, Upstreams: upOpts, Logging: qlLogs,
		})
		if err != nil {
			return err
		}
		defer func() {
			if err := complete.Close(context.Background()); err != nil {
				logger.Error("complete runtime shutdown", "error", err)
			}
		}()
		auth.Update = complete.primary.Update
		auth.Notify = complete.primary.doctorNotify(servingbuild.CapturedNotify(notifyReceiver, time.Now))
		if err := servingbuild.BindDNS(complete.publisher, servingbuild.DNSStages{Chain: chain, Control: controlStage, Identify: idStage, Authority: auth, Blocking: bs, Cache: cs, Response: post, Resolution: fwd}, time.Now); err != nil {
			return err
		}
		loggingObserver.Capture = func(req *pipeline.Request) (querylog.Collector, error) {
			return servingdns.Component[*querylog.Captured](req, "logging")
		}
		checker.Register("serving", complete.ready)
		zoneReads, err = servingbuild.NewDiagnostics(complete.publisher, time.Now)
		if err != nil {
			return err
		}
		capturedZoneReads = zoneReads
		reg.MustRegister(newServingZoneCollector(zoneReads))
		httpAuth, err := newServingHTTP(servingHTTPOptions{
			Runtime: complete, Cookie: ids.Cookie(), CrossOrigin: crossOrigin,
			ClientIP: apiClientIP, Hasher: users.NewHasher(0),
		})
		if err != nil {
			return err
		}
		httpAuth.opts.ProviderHandler = httpAuth.CapturedBrowserHandler()
		authenticate = func(next http.Handler) http.Handler {
			return httpAuth.Middleware(servingMutationRoutes(next))
		}
		authorizer, roles = httpAuth.Authorizer(), httpAuth.Roles
		authHandler = httpAuth.Handler()
		dnsQueryCapture = func(req *pipeline.Request) (pipeline.ServingLease, error) {
			lease, err := servinghttp.CloneLease(req.Ctx)
			return servingdns.Wrap(lease), err
		}
		canonicalSettings, objectCommand = complete, complete.ObjectCommand
		brandingReads = complete.Branding
		blockPagePreview = servingBrandingPreview{runtime: complete, server: blockPage, cache: &brandCache}
		nodeConfigCommand, err = newServingNodeCommand(complete, reload, logLevel)
		if err != nil {
			return err
		}
		cacheFlushCommand, err = newServingCacheCommand(complete, rc)
		if err != nil {
			return err
		}
		objectReads = servingbuild.NewObjectReads()
		infoMessageReads = servingbuild.NewInfoMessageReads()
		sourceBodyReads, err = servingbuild.NewSourceBodyReads(lists)
		if err != nil {
			return err
		}
		policyListReads = servingbuild.NewPolicyListReads()
		blockingReads = servingbuild.NewBlockingReads()
		blockingCommand = complete.BlockingCommand
		upstreamReads = servingbuild.NewUpstreamReads()
		migrationCommand = complete.MigrationCommand
		migrationReads = servingbuild.NewMigrationReads()
		zoneCommand = complete.ZoneCommand
		unblockCommand = complete.UnblockCommand
		domainCommand = complete.DomainCommand
		zoneNotifyCommand = complete.ZoneNotifyCommand
		catalogSource.Read = servingbuild.NewCatalogueReads().Catalogue
	}

	// The optional central database (D158): opened here, connected and
	// migrated in the background by its runnable; never part of readiness.
	var centralDB *central.DB
	var centralState api.CentralState
	if cfg.Database.Enabled() {
		if centralDB, err = central.Open(central.Options{DSN: cfg.Database.DSN, MaxConns: int32(cfg.Database.MaxConns), // #nosec G115 -- bounded 1-32 by the schema
			Node: cfg.Node.Name, Version: info.Version, Registerer: reg, Logger: logger}); err != nil {
			return err
		}
		defer centralDB.Close()
		centralState = centralDB
	}

	// REST API, dashboard and the external-dns webhook (§10, §5.6).
	// The sqlite store keeps audit events in state.db; the kube driver's
	// are in this node's audit spool, so /audit shows the events recorded
	// by the replica that answers.
	recorders := audit.Multi{audit.Log{Logger: logger}}
	var auditReader audit.Reader
	if ss, ok := st.(*sqlite.Store); ok {
		auditReader = auditstore.SQLite{Store: ss}
		recorders = audit.Multi{
			audit.Gated{Inner: auditstore.SQLite{Store: ss}, Allowed: func() bool { return lic.Allows(license.FeatureAudit) }},
			audit.Log{Logger: logger},
		}
	}
	var auditLogs *auditLogStore
	var auditDiagnosis func(context.Context) auditlog.Diagnostic
	if auditSpool != nil {
		auditReader = auditSpool
		recorders = audit.Multi{audit.Log{Logger: logger}, auditSpool}
		// audit.store auditlog: every replica reads every node's events.
		if cfg.AuditStore() == config.AuditStoreAuditLog {
			auditDiagnosis = auditLogDiagnosis(restCfg)
			auditKeys, keyErr := auditkeys.NewSource(restCfg, kube.PodNamespace(), cfg.Audit.KeySecret, func() string { return st.Info().ClusterID })
			if keyErr != nil {
				return keyErr
			}
			if auditLogs, err = openAuditLog(ctx, restCfg, auditSpool, auditKeys, logger, kubeStartupTimeout(cfg)); err != nil {
				return fmt.Errorf("audit: %w", err)
			}
			if auditLogs != nil {
				auditReader = auditLogs.reader
			}
		}
	}
	// audit.store database (D159): every node ships its own audit (the
	// spool, or state.db) into the central chain; every replica reads it.
	var auditShipper *audit.Shipper
	var centralJobs []central.Job
	if centralDB != nil && cfg.AuditStore() == config.AuditStoreDatabase {
		var src audit.Source
		if auditSpool != nil {
			src = auditSpool
		} else if ss, ok := st.(*sqlite.Store); ok {
			if src, err = audit.NewStateSource(ctx, ss, cfg.Node.Name); err != nil {
				return fmt.Errorf("audit: %w", err)
			}
		}
		if src != nil {
			auditShipper = &audit.Shipper{Source: src, Target: &pgaudit.Target{DB: centralDB, Version: info.Version},
				Delay: time.Second, MinGap: -1, Logger: logger}
			auditReader = &pgaudit.Reader{DB: centralDB}
			centralJobs = append(centralJobs, pgaudit.PruneJob(cfg.Audit.Retention.Std()))
		}
	}
	if auditLogs != nil {
		auditShipper = auditLogs.shipper
	}
	// The query log in the central database (D160): this node copies its
	// querylog.db rows and rollups; the leader keeps the partitions.
	var qlShipper func(context.Context) error
	var centralQuerylog *sqlitelog.Reader
	var centralPurger api.QuerylogPurger
	if centralDB != nil && cfg.Database.Querylog.Ship {
		centralQuerylog = sqlitelog.NewCentralReader(centralDB.SQL, sqlitelog.CentralOptions{
			RawRetention: cfg.Database.Querylog.Retention.Raw.Std(), DomainsOnLists: listedDomains, Recovery: pgquerylog.Recovery{DB: centralDB}})
		centralPurger = pgquerylog.Purger{DB: centralDB}
		qlShipper = func(ctx context.Context) error {
			w := qlLogs.waitWriter(ctx)
			if w == nil {
				return nil
			}
			shipper := &pgquerylog.Shipper{DB: centralDB, Local: w, Node: cfg.Node.Name, Version: info.Version,
				Facts: cfg.Database.Querylog.Facts, Retention: cfg.Database.Querylog.Retention.Raw.Std(), Registerer: reg, Logger: logger}
			return shipper.Run(ctx)
		}
		centralJobs = append(centralJobs, pgquerylog.Jobs(cfg.Database.Querylog.Retention.Raw.Std())...)
	}
	if !dashboard.Built() {
		logger.Warn("dashboard bundle not built into this binary; serving a placeholder page (make dashboard-build)")
	}
	edgeSupervisors, vipSupervised, err := edgeStatusSource(restCfg, cfg.Node.Name)
	if err != nil {
		return err
	}
	var dhcpCluster api.DHCPCluster
	if dhcpKube != nil {
		dhcpCluster = dhcpKube
	}
	apiSrv, err := api.New(api.Options{
		DHCP:            dhcpServer,
		DHCPKube:        dhcpCluster,
		DHCPSetup:       dhcpSetup,
		DHCPSetupError:  dhcpSetupError,
		EdgeSupervisors: edgeSupervisors, VIPSupervised: vipSupervised,
		Store: st, Validator: v, Cache: zc, Policy: polSrc, Blocking: bs,
		ZoneReads: capturedZoneReads, CanonicalSettings: canonicalSettings, ObjectCommand: objectCommand, ObjectReads: objectReads, ZoneCommand: zoneCommand, DomainCommand: domainCommand, MigrationCommand: migrationCommand, MigrationReads: migrationReads, UnblockCommand: unblockCommand, PolicyListReads: policyListReads,
		BlockingReads: blockingReads, BlockingCommand: blockingCommand, UpstreamReads: upstreamReads, ZoneNotifyCommand: zoneNotifyCommand,
		SourceBodyReads: sourceBodyReads, InfoMessageReads: infoMessageReads, BrandingReads: brandingReads,
		Querylog: qlLogs.reader(), QuerylogSource: qlLogs.reader, Tail: qlTail, CentralQuerylog: centralQuerylog, CentralPurger: centralPurger,
		Upstreams:     api.UpstreamStatusFunc(func() []upstream.ServerStatus { return fwd.Set().Status() }),
		ResponseCache: rc, CacheFlushCommand: cacheFlushCommand, CacheInspect: newCacheInspector(cs, fwd, dnsQueryCapture), Chain: chain, DNSQueryCapture: dnsQueryCapture,
		Config: cfg, Reload: reload, NodeConfigCommand: nodeConfigCommand, Settings: settingsWriter(cfg), SettingsOwnership: settingsManager, Objects: loader, Health: checker, Membership: membership,
		InternalClient: internalClient, License: lic, Secondaries: secondaries, Stubs: stubs,
		Lists: listResolver, Notifier: manualNotify,
		Refresher:  listRefresher(membership, fetcher),
		ListBodies: listBodies{cache: lists},
		Purger:     qlLogs,
		Audit:      recorders, AuditReader: auditReader, Central: centralState, AuditLogDiagnosis: auditDiagnosis,
		Authenticate: authenticate, Authorizer: authorizer, CSRF: authn.CSRF, CrossOrigin: crossOrigin,
		AuthHandler: authHandler,
		Roles:       roles,
		UIHandler:   dashboard.Handler(dashboard.Options{BasePath: cfg.Listeners.API.PathPrefix}),
		Logger:      logger, Registerer: reg, Version: info, NodeName: cfg.Node.Name,
		DefaultNamespace: namespace,
		Catalogue:        catalogSource,
		BlockPage:        blockPagePreview,
		Unblock:          unblockBook,
	})
	if err != nil {
		return err
	}
	var edns *externaldns.Server
	if cfg.ExternalDNS.Webhook.Enabled {
		if err := lic.Require(license.FeatureIntegrations); err != nil {
			// The webhook is an optional add-on: leave it off, keep serving.
			logger.Warn("externalDNS.webhook.enabled is set but the webhook is not started", "error", err)
		}
	}
	if cfg.ExternalDNS.Webhook.Enabled && lic.Allows(license.FeatureIntegrations) {
		eo := externaldns.OptionsFrom(cfg.ExternalDNS.Webhook)
		eo.Store, eo.Validator, eo.Cache, eo.Registerer, eo.Logger = st, v, zc, reg, logger
		if edns, err = externaldns.New(eo); err != nil {
			return err
		}
	}

	// Controller: leader-only reconcilers (status, serials, Events).
	ctrlOpts := controller.Options{Events: controller.LogSink{Logger: logger}, Registerer: reg, Logger: logger, KnownService: catalogs.HasService, KnownCategory: catalogs.HasCategory, ServicesChanged: catalogs.Subscribe()}
	if complete != nil {
		ctrlOpts.CommittedPrimaryStatus = complete.status
	}
	ctrl, err := controller.New(st, ctrlOpts)
	if err != nil {
		return err
	}

	// Runner: always-on runnables on every replica, leader-only ones under
	// the election (D44, D78). Leadership loss never ends Run.
	runner, err := cluster.New(cluster.Options{
		Elector:          st.Elector(cluster.ElectionController),
		Lameduck:         func(context.Context) { checker.Lameduck() },
		LameduckDuration: cfg.Node.Lameduck.Std(),
		JoinShutdown:     complete != nil,
		Registerer:       reg, Logger: logger,
	})
	if err != nil {
		return err
	}
	add := func(name string, leaderOnly bool, fn func(context.Context) error) error {
		return runner.Add(name, leaderOnly, fn)
	}
	if complete != nil {
		if err := add("serving", false, complete.Run); err != nil {
			return err
		}
		if err := add("serving-writer", true, complete.RunWriter); err != nil {
			return err
		}
		if err := add("rpz", false, feeds.Run); err != nil {
			return err
		}
	} else if err := add("notifier", true, nt.Run); err != nil {
		return err
	}
	if complete == nil {
		if err := add("settings", false, func(ctx context.Context) error {
			opts := config.WatchOptions{Environ: os.Environ(), Logger: logger}
			if settingsManager != nil {
				opts.Refresh = settingsManager.Refresh
			}
			return config.WatchSettings(ctx, reload, opts)
		}); err != nil {
			return err
		}
	}
	if seedPending {
		if err := add("bootstrap", false, func(ctx context.Context) error {
			for {
				if err := seedDefaults(ctx); err == nil {
					return nil
				} else if ctx.Err() != nil {
					return nil
				} else if !kube.IsUnavailable(err) {
					return err
				}
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(time.Second):
				}
			}
		}); err != nil {
			return err
		}
	}
	for _, r := range []struct {
		name       string
		leaderOnly bool
		fn         func(context.Context) error
	}{
		{"license", false, func(ctx context.Context) error { return lic.Run(ctx, licSources, time.Minute, logger) }},
		{"zonecache", false, zc.Start},
		{"secondaries", false, secondaries.Run},
		{"stubs", false, stubs.Run},
		{"secondary-notify-sources", false, notifyReceiver.Run},
		{"secondary-notify-fanout", false, notifyFanout.Run},
		{"objects", sql != nil, func(ctx context.Context) error {
			if sql != nil {
				loader.Load(ctx) // every original writer starts with its local manifest
			}
			return loader.Watch(ctx)
		}},
		{"dns", false, registry.Run},
		{"ops", false, func(ctx context.Context) error {
			return serveHTTP(ctx, cfg.Listeners.Ops.Address, checker.Handler(), logger, "ops")
		}},
		{"metrics", false, func(ctx context.Context) error {
			mux := http.NewServeMux()
			mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{EnableOpenMetrics: true}))
			return serveHTTP(ctx, cfg.Listeners.Metrics.Address, mux, logger, "metrics")
		}},
		{"selfcheck", false, func(ctx context.Context) error {
			initialized := zc.Initialized()
			if complete != nil {
				initialized = complete.publisher.Initialized()
			}
			select {
			case <-initialized:
				checker.SetStarted()
			case <-ctx.Done():
				return nil
			}
			t := time.NewTicker(5 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return nil
				case <-t.C:
					checker.Tick()
				}
			}
		}},
		{"controller", true, ctrl.Run},
		{"listfetcher", true, fetcher.Run},
		{"listresolver", false, listResolver.Run},
		{"identity", false, ids.Run},
		{"api", false, apiSrv.Run},
		{"unblock", false, unblockBook.Run},
		{"unblock-janitor", true, func(ctx context.Context) error {
			if complete != nil {
				return unblockBook.RunCurrentJanitor(ctx, func(ctx context.Context) (unblock.SweepResult, error) {
					return complete.SweepUnblock(ctx)
				})
			}
			return unblockBook.RunJanitor(ctx)
		}},
		{"domainlist", true, domainlist.New(domainlist.Options{Store: st, Logger: logger}).Run},
	} {
		if err := add(r.name, r.leaderOnly, r.fn); err != nil {
			return err
		}
	}
	if r := catRefreshers.document; r != nil {
		if err := add("catalogue", false, r.Run); err != nil {
			return err
		}
	}
	if r := catRefreshers.adguard; r != nil {
		if err := add("catalogue-adguard", false, r.Run); err != nil {
			return err
		}
	}
	reloadConsumers := reloadTargets{
		listFetcher: fetcher, listResolver: listResolver,
		logger: logger, level: logLevel, upstreams: fwd, upOpts: upOpts, cache: rc, cacheStg: cs,
		control: controlStage, blocking: bs, blockPage: blockPage, edgeFlush: edgeFlush, limiter: limiter, qlOwner: qlLogs, postprocess: post,
	}
	if complete != nil {
		// Complete requests use immutable settings and resource owners. Legacy
		// reload callbacks must not replace or close their serving resources.
		reloadConsumers.upstreams, reloadConsumers.cache, reloadConsumers.cacheStg = nil, nil, nil
		reloadConsumers.control, reloadConsumers.blocking, reloadConsumers.postprocess = nil, nil, nil
		// The current node command owns local logger adoption. Cluster
		// operational refreshes must not restore the startup log level.
		reloadConsumers.level = nil
		if err := add("serving-settings", false, func(ctx context.Context) error {
			return complete.runServingReload(ctx, reloadConsumers, rc)
		}); err != nil {
			return err
		}
	} else {
		subscribeReload(ctx, reload, reloadConsumers)
	}
	if rc != nil {
		if err := add("cache-sweep", false, func(ctx context.Context) error { return rc.Run(ctx, time.Minute) }); err != nil {
			return err
		}
	}
	if cachePersister != nil {
		if err := add("cache-persist", false, func(ctx context.Context) error {
			return cache.RunPersister(ctx, rc, cachePersister, 10*time.Minute, 5*time.Second, logger)
		}); err != nil {
			return err
		}
	}
	if internalSrv != nil {
		if err := add("internal", false, internalSrv.Run); err != nil {
			return err
		}
	}
	if p := cfg.Auth.AdminSocket.Path; p != "" {
		if err := add("adminsock", false, func(ctx context.Context) error {
			err := ids.AdminSocket().ListenAndServe(ctx, p)
			if errors.Is(err, adminsock.ErrBusy) {
				// Another kudurru on this host owns the default socket: keep
				// serving without `kudurru admin` instead of failing startup.
				logger.Warn("admin socket is in use by another process; `kudurru admin` will not reach this node (set auth.adminSocket.path)", "path", p)
				<-ctx.Done()
				return nil
			}
			return err
		}); err != nil {
			return err
		}
	}
	if edns != nil {
		if err := add("externaldns", false, edns.Run); err != nil {
			return err
		}
	}
	if cfg.BlockPage.Enabled {
		if err := add("blockpage", false, blockPage.Run); err != nil {
			return err
		}
	}
	if dhcpServer != nil {
		if err := add("dhcp", false, dhcpServer.Run); err != nil {
			return err
		}
	}
	if dhcpKube != nil {
		if err := add("dhcp", false, dhcpKube.Run); err != nil {
			return err
		}
	}
	if err := add("querylog", false, qlLogs.run); err != nil {
		return err
	}
	// Audit retention (§11.4): the leader prunes state.db audit rows older
	// than 400 days once a day.
	if centralDB != nil {
		if err := add("central", false, centralDB.Run); err != nil {
			return err
		}
		if len(centralJobs) > 0 {
			if err := add("central-janitor", true, (&central.Janitor{DB: centralDB, Jobs: centralJobs, Logger: logger}).Run); err != nil {
				return err
			}
		}
	}
	if auditShipper != nil {
		if err := add("audit-ship", false, auditShipper.Run); err != nil {
			return err
		}
	}
	if qlShipper != nil {
		if err := add("querylog-ship", false, qlShipper); err != nil {
			return err
		}
	}
	auditRetention := cfg.Audit.Retention.Std()
	if ss, ok := st.(*sqlite.Store); ok {
		if err := add("audit-prune", true, (&sqlite.AuditPruner{Store: ss, Retention: auditRetention, Logger: logger}).Run); err != nil {
			return err
		}
	}
	// Every node prunes its own audit spool; the leader prunes the
	// AuditLog batches, which every node ships its spool into.
	if auditSpool != nil {
		if err := add("audit-spool-prune", false, (&audit.Pruner{Name: "spool", Prune: auditSpool.Prune, Retention: auditRetention, Logger: logger}).Run); err != nil {
			return err
		}
	}
	if auditLogs != nil {
		if err := add("auditlog-prune", true, (&audit.Pruner{Name: "auditlog", Prune: auditLogs.prune, Retention: auditRetention, Logger: logger}).Run); err != nil {
			return err
		}
	}

	runErr := runner.Run(ctx)
	if runErr != nil {
		logger.Error("component failed, shutting down", "err", runErr)
	}

	if cs != nil {
		cs.Wait()
	}
	_ = fwd.Set().Close()

	if snapDB != nil {
		_ = snapDB.Close()
	}
	logger.Info("kudurru stopped")
	return runErr
}

func serveHTTP(ctx context.Context, addr string, h http.Handler, logger *slog.Logger, name string) error {
	srv := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 5 * time.Second}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	logger.Info(name+" listener started", "address", addr)
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}

// readEncryptionKeys reads KUDURRU_ENCRYPTION_KEY[_FILE] and the _PREVIOUS
// variants from the environment only, never generating one: the edge and
// VIP supervisors run in their own images and must share the node's key.
func readEncryptionKeys() (key, previous []byte, err error) {
	read := func(name string) ([]byte, error) {
		v := os.Getenv(name)
		if f := os.Getenv(name + "_FILE"); f != "" {
			b, err := os.ReadFile(f) //nolint:gosec // the operator names the file (KUDURRU_*_FILE)
			if err != nil {
				return nil, fmt.Errorf("%s_FILE: %w", name, err)
			}
			v = strings.TrimSpace(string(b))
		}
		if v == "" {
			return nil, nil
		}
		k, err := session.ParseKey(v)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		return k, nil
	}
	if key, err = read(keys.EnvKey); err != nil {
		return nil, nil, err
	}
	if previous, err = read(keys.EnvPreviousKey); err != nil {
		return nil, nil, err
	}
	return key, previous, nil
}

// identityAuthentication keeps migration admission outside the entire remote
// authentication stack, including anonymous login and callback requests.
func identityAuthentication(ids *stack.Stack, gate *iamgate.Gate) func(http.Handler) http.Handler {
	if gate == nil {
		return ids.Middleware()
	}
	return func(next http.Handler) http.Handler {
		return gate.Middleware(ids.Middleware()(next))
	}
}

// identityOptions are the stack.Options that do not depend on the
// deployment shape (the Kubernetes config, namespace and internal token
// are added by the caller).
func identityOptions(cfg *config.Config, st store.Store, k keys.Keys, logger *slog.Logger) stack.Options {
	return stack.Options{
		Config: cfg, Store: st, Keys: k,
		TrustForwardedProto: len(cfg.Listeners.API.TrustedProxies) > 0,
		APIPrefix:           strings.TrimSuffix(cfg.Listeners.API.PathPrefix, "/") + "/api/v1",
		RoutePrefix:         stack.DefaultAPIPrefix,
		ClientIP:            apiClientIP,
		Logger:              logger,
	}
}

// apiClientIP is stack.Options.ClientIP: the client address the API
// server's RealIP step derived (the right-most untrusted X-Forwarded-For
// entry behind listeners.api.trustedProxies), so login lockouts count
// clients rather than the reverse proxy every request arrives from (and
// API tokens record the client as lastUsedIP).
// Requests that did not pass the API server (the admin socket) fall back
// to the transport peer.
func apiClientIP(r *http.Request) string {
	if a := api.ClientIP(r.Context()); a.IsValid() {
		return a.String()
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// flushingListSink hands every compiled list snapshot to the zone cache and
// then flushes the local edge's resolver cache.
type flushingListSink struct {
	next  listfetcher.ListSink
	flush *dnsdist.CacheFlusher
}

func (s flushingListSink) SetLists(sn *blocking.Snapshot) {
	s.next.SetLists(sn)
	s.flush.ListsChanged()
}

// domainsOnLists is the dashboard's "domains on lists" tile: the compiled
// block entries of every block-mode list, without the lists' own @@
// exceptions. Allow lists do not count. nil (no lists compiled yet) is 0.
func domainsOnLists(sn *blocking.Snapshot) int64 {
	if sn == nil {
		return 0
	}
	var n int64
	for _, l := range sn.Lists() {
		if l.Mode() != blocking.ModeBlock {
			continue
		}
		st := l.Stats()
		n += int64(st.Entries - st.Exceptions)
	}
	return n
}
