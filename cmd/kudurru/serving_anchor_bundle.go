package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"time"

	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingbuild"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/accepted"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/resolverstate"
)

const recursiveRootComponent = "recursive-root"

// rootAnchorOwner is created only by root composition from its actual Store.
// It never initializes history. Host is the runtime's detached immutable copy.
type rootAnchorOwner struct {
	store       *sqlite.Store
	source      *sqlite.AnchorSource
	host        *config.Config
	incarnation [32]byte
	admission   *rootAnchorAdmission
}

// The coalesced signal wakes rebuilding; it never grants output admission.
func (o *rootAnchorOwner) watch(ctx context.Context, wake func()) error {
	changed := o.source.Changes()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
			wake()
		}
	}
}

func newRootAnchorOwner(ctx context.Context, store *sqlite.Store, host *config.Config) (*rootAnchorOwner, error) {
	if ctx == nil || store == nil || host == nil {
		return nil, serving.ErrComponents
	}
	original, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if store.Authority().Writable() {
		writer, err := store.WriterToken()
		if err != nil {
			return nil, err
		}
		original = sqlite.WithWriterToken(original, writer)
	}
	source, err := store.BindAnchorLifecycle(original)
	if err != nil {
		return nil, err
	}
	owner := &rootAnchorOwner{store: store, source: source, host: host}
	if _, err := rand.Read(owner.incarnation[:]); err != nil {
		return nil, err
	}
	return owner, nil
}

// A library Input or Projection cannot construct this private root component.
// The component retains only its original immutable complete preparation and
// mechanical source. It deliberately does not retain the preparation context.
type recursiveRootSeal struct {
	base              replication.Generation
	dependencyCurrent func() bool
	managed           *sqlite.AnchorResolverInput
	static            *sqlite.AnchorStaticInput
	admission         *rootAnchorAdmission
}

func (s *recursiveRootSeal) current() bool {
	if s == nil || s.dependencyCurrent == nil || !s.dependencyCurrent() {
		return false
	}
	if s.managed != nil {
		return s.static == nil && s.admission.current() && s.managed.Current()
	}
	return s.static != nil && s.static.Current()
}

// prepare invokes the concrete builder, then verifies the complete original cut
// before appending the root-only component. Ownership still transfers on error,
// so the publisher releases every successfully prepared component exactly once.
func (o *rootAnchorOwner) prepare(ctx context.Context, candidate serving.Candidate, builder *servingbuild.Builder) (serving.Prepared, error) {
	if o == nil || o.source == nil || builder == nil || ctx == nil {
		return serving.Prepared{}, serving.ErrComponents
	}
	deadline, finite := ctx.Deadline()
	if !finite || time.Until(deadline) > 30*time.Second {
		return serving.Prepared{}, errAnchorPolicy
	}
	if err := ctx.Err(); err != nil {
		return serving.Prepared{}, err
	}
	prepared, err := builder.Prepare(ctx, candidate)
	if err != nil {
		return prepared, err
	}
	if candidate.Image == nil || !candidate.Image.Generation().Same(candidate.Token.Generation()) || prepared.Current == nil || !prepared.Current() {
		return prepared, serving.ErrComponents
	}
	base := candidate.Token.Generation()
	parts := make(map[string]bool, len(prepared.Components))
	for _, part := range prepared.Components {
		if parts[part.Name] || part.Name == recursiveRootComponent || part.Identity == "" || !part.Base.Same(base) || rootAnchorNil(part.Value) {
			return prepared, serving.ErrComponents
		}
		parts[part.Name] = true
	}
	for _, required := range serving.CompleteManifest() {
		if !parts[required] {
			return prepared, serving.ErrComponents
		}
	}
	settings, err := anchorImageSettings(candidate.Image)
	if err != nil {
		return prepared, err
	}
	document, err := servingSettingsDocument(settings)
	if err != nil {
		return prepared, err
	}
	effective, _, err := config.ApplyClusterDocument(o.host, document)
	if err != nil {
		return prepared, err
	}
	policy, err := effective.Upstreams.Recursion.RootAnchorPolicy()
	if err != nil {
		return prepared, err
	}
	owner, err := accepted.DecodeRows(candidate.Image.Payload.Runtime, base.Term.ClusterID)
	if err != nil {
		return prepared, err
	}
	seal := &recursiveRootSeal{base: base, dependencyCurrent: prepared.Current}
	var domain [32]byte
	switch policy.Mode {
	case config.RootAnchorsStatic:
		if owner.Inspect().Phase != accepted.Dormant || prepared.Anchor != nil {
			return prepared, errAnchorPolicy
		}
		seal.static, err = o.source.StaticInput()
		if err != nil {
			return prepared, err
		}
	case config.RootAnchorsManaged:
		if prepared.Anchor == nil || !o.admission.current() {
			return prepared, errAnchorPolicy
		}
		seal.admission = o.admission
		projection := prepared.Anchor.Projection()
		identity := projection.Identity()
		if projection.Disposition() != resolverstate.Usable || len(projection.DS()) == 0 ||
			identity.ClusterID != base.Term.ClusterID || identity.Owner != owner.Version() ||
			identity.PolicyID != policy.PolicyID || identity.SeedDigest != policy.SeedDigest {
			return prepared, errAnchorPolicy
		}
		seal.managed = prepared.Anchor.Input()
		if seal.managed == nil {
			return prepared, errAnchorPolicy
		}
		domain = seal.managed.CacheDomain()
		if domain == ([32]byte{}) {
			return prepared, errAnchorPolicy
		}
	default:
		return prepared, errAnchorPolicy
	}
	// This inert original-token check establishes this owner's actual Store;
	// it does not acquire R or grant publication. The real complete swap still
	// applies the original managed registration under its own mode/R hold.
	if err := o.store.PublishIfCurrent(ctx, candidate.Token, sqlite.PublicationOptions{}, func() error {
		if !seal.current() {
			return serving.ErrDependency
		}
		return nil
	}); err != nil {
		return prepared, err
	}
	raw, err := json.Marshal(struct {
		Base        replication.Generation
		Policy      string
		Seed        string
		Domain      [32]byte
		Incarnation [32]byte
	}{base, policy.PolicyID, policy.SeedDigest, domain, o.incarnation})
	if err != nil {
		return prepared, err
	}
	digest := sha256.Sum256(raw)
	if err := ctx.Err(); err != nil {
		return prepared, err
	}
	if !seal.current() {
		return prepared, serving.ErrDependency
	}
	prepared.Components = append(prepared.Components, serving.Component{Name: recursiveRootComponent, Base: base, Identity: hex.EncodeToString(digest[:]), Value: seal})
	prepared.Current = seal.current
	return prepared, nil
}

func rootAnchorNil(value any) bool {
	if value == nil {
		return true
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}

// Each read belongs to the original complete lease and is captured before its
// query/output/cache acquisition. No method captures a replacement on refusal.
type recursiveRootRead struct {
	seal    *recursiveRootSeal
	managed *sqlite.AnchorRead
	static  *sqlite.AnchorStaticRead
}

func (s *recursiveRootSeal) captureRead(ctx context.Context) (*recursiveRootRead, error) {
	if !s.current() {
		return nil, serving.ErrDependency
	}
	read := &recursiveRootRead{seal: s}
	var err error
	if s.managed != nil {
		read.managed, err = s.managed.CaptureRead(ctx)
	} else {
		read.static, err = s.static.CaptureRead(ctx)
	}
	if err != nil {
		return nil, err
	}
	return read, nil
}

// withRead holds only mode/R around the synchronous final handoff. The caller
// must finish actual writes/inserts before returning; no Store/publisher calls,
// new acquisition or resource release belongs inside. Check is borrowed only.
func (s *recursiveRootSeal) withRead(read *recursiveRootRead, fn func(func() error) error) error {
	return s.withReadCurrent(read, s.current, fn)
}

// Only a root-issued UPDATE completion may substitute the original writer and
// dependency check for data-generation currency. The original mode/R attempt
// and its lifetime remain mandatory for both kinds of emission.
func (s *recursiveRootSeal) withReadCurrent(read *recursiveRootRead, dependencyCurrent func() bool, fn func(func() error) error) error {
	if s == nil || read == nil || read.seal != s || fn == nil || dependencyCurrent == nil {
		return serving.ErrComponents
	}
	invoke := func(check func() error) error {
		current := func() error {
			if err := check(); err != nil {
				return err
			}
			if !dependencyCurrent() {
				return serving.ErrDependency
			}
			return nil
		}
		if err := current(); err != nil {
			return err
		}
		if err := fn(current); err != nil {
			return err
		}
		return current()
	}
	if s.managed != nil {
		return s.managed.WithRead(read.managed, func(scope sqlite.AnchorReadScope) error { return invoke(scope.Check) })
	}
	if s.static == nil {
		return serving.ErrComponents
	}
	return s.static.WithRead(read.static, func(scope sqlite.AnchorStaticScope) error { return invoke(scope.Check) })
}
