package host

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/ee/integration"
	"github.com/robertherbaugh/kudurru/internal/inventory"
)

// negotiatedProvider is one Go type that implements every host role, like
// the Tier-B webhook provider; its peer decides which roles are real.
type negotiatedProvider struct {
	*fakePublisher
	roles     Roles
	rolesErr  error
	cadence   time.Duration
	observed  atomic.Int32
	delivered atomic.Int32
}

func (p *negotiatedProvider) Roles() (Roles, error) { return p.roles, p.rolesErr }
func (p *negotiatedProvider) ObservationSchedule() (time.Duration, error) {
	return p.cadence, nil
}
func (p *negotiatedProvider) Observe(context.Context) (SourceSnapshot, error) {
	p.observed.Add(1)
	return SourceSnapshot{Complete: true}, nil
}
func (*negotiatedProvider) Topics() []string { return []string{"zone.changed"} }
func (p *negotiatedProvider) HandleDelivery(context.Context, string, integration.Event) error {
	p.delivered.Add(1)
	return nil
}

// onlyObserver implements Observer but not Publisher or DeliveryHook.
type onlyObserver struct {
	*fakePublisherBase
	roles Roles
}
type fakePublisherBase struct{ integration.Integration }

func (p *onlyObserver) Roles() (Roles, error) { return p.roles, nil }
func (*onlyObserver) Observe(context.Context) (SourceSnapshot, error) {
	return SourceSnapshot{Complete: true}, nil
}

func TestNegotiatedRolesScheduleMatrix(t *testing.T) {
	for _, tc := range []struct {
		name     string
		roles    Roles
		cadence  time.Duration
		resync   time.Duration
		enabled  bool
		interval time.Duration
		invalid  bool
	}{
		{name: "none", roles: Roles{}},
		{name: "hook_only", roles: Roles{Hook: true}},
		{name: "observer", roles: Roles{Observer: true}, cadence: time.Minute, enabled: true, interval: time.Minute},
		{name: "observer_shorter_resync", roles: Roles{Observer: true}, cadence: time.Minute, resync: 5 * time.Second, enabled: true, interval: 5 * time.Second},
		{name: "observer_without_cadence", roles: Roles{Observer: true}, invalid: true},
		{name: "publisher", roles: Roles{Publisher: true}, enabled: true, interval: 10 * time.Minute},
		{name: "publisher_and_observer", roles: Roles{Publisher: true, Observer: true, Hook: true}, cadence: time.Minute, enabled: true, interval: time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			definition := webhook("roles")
			definition.Spec.Resync.Duration = tc.resync
			p := &negotiatedProvider{fakePublisher: &fakePublisher{}, roles: tc.roles, cadence: tc.cadence}
			enabled, interval, err := syncSchedule(p, definition)
			if (err != nil) != tc.invalid || enabled != tc.enabled || interval != tc.interval {
				t.Fatalf("schedule: enabled=%t interval=%s err=%v", enabled, interval, err)
			}
		})
	}
	// A declared role the Go type cannot serve, or a refused answer, is never
	// a schedule.
	if _, _, err := syncSchedule(&onlyObserver{fakePublisherBase: &fakePublisherBase{&fakePublisher{}}, roles: Roles{Hook: true}}, webhook("x")); !errors.Is(err, ErrInvalid) {
		t.Fatal("impossible hook role accepted", err)
	}
	if _, _, err := syncSchedule(&negotiatedProvider{fakePublisher: &fakePublisher{}, rolesErr: ErrFenced}, webhook("x")); !errors.Is(err, ErrFenced) {
		t.Fatal("refused roles scheduled", err)
	}
	// Without RoleProvider the type assertions decide, as before: a type that
	// is both a scheduler and a publisher stays invalid.
	legacy := &scheduledObserver{Integration: &fakePublisher{}, interval: time.Minute}
	if _, _, err := syncSchedule(&struct {
		*scheduledObserver
		*fakePublisher
	}{legacy, &fakePublisher{}}, webhook("x")); err == nil {
		t.Fatal("legacy scheduler+publisher became valid")
	}
}

func TestNegotiatedRolesExecute(t *testing.T) {
	for _, tc := range []struct {
		name    string
		roles   Roles
		observe int32
	}{
		{name: "observer_only_never_publishes", roles: Roles{Observer: true}, observe: 1},
		{name: "hook_only_never_polls", roles: Roles{Hook: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, j, fake, _, _ := engineFixture(t)
			h.opts.Inventory = inventory.New(h.opts.Now)
			p := &negotiatedProvider{fakePublisher: fake, roles: tc.roles, cadence: time.Minute}
			j.provider = p
			a := h.execute(t.Context(), j, command{kind: "sync"})
			if tc.observe == 0 && !errors.Is(a.err, ErrUnsupported) || tc.observe != 0 && a.err != nil {
				t.Fatalf("sync: %v", a.err)
			}
			if p.observed.Load() != tc.observe || fake.calls != 0 {
				t.Fatalf("observed %d times, applied %d times", p.observed.Load(), fake.calls)
			}
			// The Go type is a Publisher, but its peer did not negotiate it.
			if a := h.execute(t.Context(), j, command{kind: "preview"}); !errors.Is(a.err, ErrUnsupported) {
				t.Fatalf("preview of a non-publisher: %v", a.err)
			}
		})
	}
}

// Reviewed (D401) qualifies only a publisher role.
func TestReviewedRoleRequiresPublisher(t *testing.T) {
	if _, err := providerRoles(&negotiatedProvider{fakePublisher: &fakePublisher{}, roles: Roles{Observer: true, Reviewed: true}}); !errors.Is(err, ErrInvalid) {
		t.Fatal("reviewed without publisher accepted", err)
	}
	if r, err := providerRoles(&negotiatedProvider{fakePublisher: &fakePublisher{}, roles: Roles{Publisher: true, Reviewed: true}}); err != nil || !r.Reviewed {
		t.Fatal("reviewed publisher refused", err)
	}
}
