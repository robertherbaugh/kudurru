package servingbuild

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"time"

	internal "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/listdata/catalogue"
	"github.com/robertherbaugh/kudurru/internal/listfetcher"
	"github.com/robertherbaugh/kudurru/internal/policy"
	"github.com/robertherbaugh/kudurru/internal/responsepolicy"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	blockingstage "github.com/robertherbaugh/kudurru/internal/stages/blocking"
	cachestage "github.com/robertherbaugh/kudurru/internal/stages/cache"
	upstreamstage "github.com/robertherbaugh/kudurru/internal/stages/upstream"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/upstream"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
)

// CatalogResource binds a prepared catalog to the exact configured source.
// Changing a URL cannot silently adopt bytes loaded for the previous source.
type CatalogResource struct {
	Catalogue *catalogue.Catalogue
	Source    config.BlockingCatalogue
}

type semanticOwners struct {
	migrations []*internal.MigrationRun
	objects    *zonecache.Snapshot
	settings   *public.Kudurru
	config     *config.Config
	policy     *policy.Index
	resolution *upstreamstage.View
	response   *responsepolicy.Settings
	cache      *cachestage.View
	blocking   *blockingstage.View
	auth       *servingauth.View
	upstreams  *upstream.Set
	lists      string

	unblockRequests []*public.UnblockRequest
}

func (s *semanticOwners) release() {
	if s == nil {
		return
	}
	if s.auth != nil {
		s.auth.Release()
	}
	if s.upstreams != nil {
		_ = s.upstreams.Close()
	}
}

func prepareSemantic(ctx context.Context, candidate serving.Candidate, opts Options, deps Dependencies, now time.Time) (_ *semanticOwners, err error) {
	owners := &semanticOwners{}
	defer func() {
		if err != nil {
			owners.release()
		}
	}()
	cut, err := servingauth.CaptureImage(ctx, candidate.Image, opts.Schema.auth())
	if err != nil {
		return nil, err
	}
	owners.auth, err = servingauth.Prepare(ctx, cut, deps.Auth)
	if err != nil {
		return nil, err
	}
	owners.settings, err = owners.auth.Settings()
	if err != nil {
		return nil, err
	}
	document := map[string]any{}
	for key, raw := range owners.settings.Spec.Settings {
		var value any
		if err := json.Unmarshal(raw.Raw, &value); err != nil {
			return nil, err
		}
		document[key] = value
	}
	owners.config, _, err = config.ApplyClusterDocument(deps.Host, document)
	if err != nil {
		return nil, err
	}
	if deps.Catalog.Catalogue == nil || !reflect.DeepEqual(deps.Catalog.Source, owners.config.Blocking.Catalogue) {
		return nil, serving.ErrDependency
	}
	// The DNS object projection is intentionally smaller than the complete
	// image used by auth and runtime. All are still bound to the same Base.
	kinds := zonecache.DefaultKinds()
	var objects []store.Object
	var lists []*public.BlockList
	for _, raw := range candidate.Image.Payload.Objects {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		object, err := opts.Schema.decode(raw)
		if err != nil {
			return nil, err
		}
		if run, ok := object.(*internal.MigrationRun); ok {
			owners.migrations = append(owners.migrations, run)
		}
		if request, ok := object.(*public.UnblockRequest); ok {
			owners.unblockRequests = append(owners.unblockRequests, request)
		}
		if slices.Contains(kinds, store.KindOf(object)) {
			objects = append(objects, object)
		}
		if list, ok := object.(*public.BlockList); ok {
			lists = append(lists, list)
		}
	}
	compiled, err := listfetcher.PrepareComplete(ctx, lists, listfetcher.CompleteOptions{Cache: opts.Lists, Catalogue: deps.Catalog.Catalogue})
	if err != nil {
		return nil, err
	}
	owners.lists = compiled.Identity
	owners.objects, err = zonecache.Prepare(zonecache.PrepareOptions{Kinds: kinds, Lists: compiled.Snapshot, Generation: candidate.Image.Revision, Now: now}, objects, nil)
	if err != nil {
		return nil, err
	}
	set := owners.objects.Objects
	owners.policy = policy.Build(policy.Input{ClientGroups: set.ClientGroups(), Policies: set.Policies(), BlockLists: set.BlockLists(), Lists: compiled.Snapshot, Generation: candidate.Image.Revision}, policy.Options{DefaultNamespace: opts.Namespace, Services: deps.Catalog.Catalogue, Inventory: true})
	// A complete publication owns its recursive resolver as well as its routes.
	// Reusing the legacy mutable resolver would ignore captured recursion settings
	// and let a reload change an older request's trust and root-hint configuration.
	upstreamOptions := opts.Upstreams
	upstreamOptions.Recursor = nil
	owners.upstreams, err = upstream.New(upstream.ConfigFrom(owners.config.Upstreams), upstreamOptions)
	if err != nil {
		return nil, err
	}
	validator, err := owners.config.DNSSEC.Validator(nil)
	if err != nil {
		return nil, err
	}
	// A concrete store-issued token is checked against memory-only authority.
	// Even A -> B -> A receives a fresh cache/resolver incarnation. Late fills
	// cannot become eligible merely because configuration bytes match again.
	current := func() bool {
		authority, blocked := opts.Authority()
		return !blocked && candidate.Token.Matches(authority) && deps.Current()
	}
	owners.resolution, err = upstreamstage.NewView(owners.upstreams, validator, owners.config.DNSSEC.Enabled, current)
	if err != nil {
		return nil, err
	}
	owners.response, err = owners.config.Response.Compile()
	if err != nil {
		return nil, err
	}
	owners.cache, err = cachestage.NewView(cachestage.SettingsFrom(owners.config.Cache), owners.config.Cache.MaxEntries == 0, current)
	if err != nil {
		return nil, err
	}
	var disabledUntil time.Time
	if status := owners.settings.Status.Blocking; status.Disabled {
		disabledUntil = blockingstage.Forever
		if status.DisabledUntil != nil {
			disabledUntil = status.DisabledUntil.Time
		}
	}
	owners.blocking, err = blockingstage.NewView(blockingstage.OptionsFromConfig(owners.config), disabledUntil, deps.Catalog.Catalogue)
	if err != nil {
		return nil, err
	}
	return owners, ctx.Err()
}

func componentIdentity(base replication.Generation, dependency, owner string) string {
	raw := fmt.Sprintf("servingbuild/v1\x00%s\x00%s\x00%s", base.Digest, dependency, owner)
	digest := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(digest[:])
}
