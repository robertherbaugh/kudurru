package servingauth

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"time"

	publicv1 "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/auth/oidccore"
)

// MaxProviders bounds effective provider compilers and verifier resources.
const MaxProviders = 64

// MaxProviderBytes bounds aggregate nested policy dependency input.
const MaxProviderBytes = 1 << 20

type compiledProvider struct {
	descriptor ProviderDescriptor
	resource   ProviderResource
	identity   string
	policy     *oidccore.Policy
}

// ProviderSources projects only actual captured object sources plus explicit host config.
func ProviderSources(ctx context.Context, cut *Cut, config []ProviderSource) ([]ProviderDescriptor, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if cut == nil || config == nil {
		return nil, ErrManifest
	}
	if len(config) > MaxProviders {
		return nil, ErrBounds
	}
	sources := map[string]ProviderSource{}
	budget := 0
	add := func(source ProviderSource) error {
		if !validProviderRef(source.Ref) || !validTime(source.CreatedAt) {
			return ErrInvalid
		}
		if err := providerBudget(reflect.ValueOf(source.Spec), &budget); err != nil {
			return err
		}
		sources[source.Ref.ID] = source
		return nil
	}
	for _, source := range config {
		if source.Ref.Origin != ProviderConfig {
			return nil, ErrInvalid
		}
		if _, ok := sources[source.Ref.ID]; ok {
			return nil, ErrInvalid
		}
		if err := add(source); err != nil {
			return nil, err
		}
	}
	count := len(config)
	for _, object := range cut.objects {
		idp, ok := object.(*publicv1.IdentityProvider)
		if !ok {
			continue
		}
		count++
		if count > 2*MaxProviders {
			return nil, ErrBounds
		}
		if idp.DeletionTimestamp != nil {
			continue
		}
		if _, shadowed := sources[idp.Name]; shadowed {
			continue
		}
		if err := add(ProviderSource{Ref: ProviderRef{ID: idp.Name, UID: string(idp.UID), Origin: ProviderObject}, CreatedAt: idp.CreationTimestamp.Time, Spec: idp.Spec}); err != nil {
			return nil, err
		}
	}
	if len(sources) > MaxProviders {
		return nil, ErrBounds
	}
	ids := make([]string, 0, len(sources))
	for id := range sources {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	result := make([]ProviderDescriptor, 0, len(ids))
	issuers := map[string]bool{}
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		source := sources[id]
		policy, err := oidccore.Compile(source.Spec, id)
		if err != nil {
			return nil, ErrInvalid
		}
		if issuers[source.Spec.Issuer] {
			return nil, ErrInvalid
		}
		issuers[source.Spec.Issuer] = true
		identity := hash(struct {
			Profile   string
			Ref       ProviderRef
			CreatedAt time.Time
			Spec      publicv1.IdentityProviderSpec
		}{"servingauth/provider/v1", source.Ref, source.CreatedAt.UTC(), source.Spec})
		result = append(result, ProviderDescriptor{Ref: source.Ref, CreatedAt: source.CreatedAt.UTC(), Issuer: source.Spec.Issuer, SpecIdentity: identity, PolicyIdentity: policy.Identity(), Spec: policy.Spec()})
	}
	return result, nil
}

// Count nested dependency input before compiler/JSON cloning allocations.
func providerBudget(v reflect.Value, budget *int) error {
	*budget += 32
	if *budget > MaxProviderBytes {
		return ErrBounds
	}
	switch v.Kind() {
	case reflect.Pointer:
		if !v.IsNil() {
			return providerBudget(v.Elem(), budget)
		}
	case reflect.Struct:
		for j := 0; j < v.NumField(); j++ {
			if err := providerBudget(v.Field(j), budget); err != nil {
				return err
			}
		}
	case reflect.Slice:
		if v.Len() > 64 {
			return ErrBounds
		}
		for j := 0; j < v.Len(); j++ {
			if err := providerBudget(v.Index(j), budget); err != nil {
				return err
			}
		}
	case reflect.String:
		if v.Len() > oidccore.MaxStringBytes {
			return ErrBounds
		}
		*budget += 6 * v.Len()
	}
	if *budget > MaxProviderBytes {
		return ErrBounds
	}
	return nil
}

// ProviderResourceIdentity checks an exact source/resource and hashes its capability.
func ProviderResourceIdentity(source ProviderDescriptor, resource ProviderResource) (string, error) {
	budget := 0
	if err := providerBudget(reflect.ValueOf(source.Spec), &budget); err != nil {
		return "", err
	}
	if !validProviderRef(source.Ref) || !validTime(source.CreatedAt) || !hashPattern.MatchString(source.SpecIdentity) || !hashPattern.MatchString(source.PolicyIdentity) || resource.Ref != source.Ref || resource.SpecIdentity != source.SpecIdentity || resource.PolicyIdentity != source.PolicyIdentity || !safeText(resource.TrustIdentity, 512, false) {
		return "", ErrDependency
	}
	switch resource.Availability {
	case ProviderReady:
		if resource.Verifier == nil || resource.Verifier.Issuer() != source.Issuer || resource.Verifier.Identity() != resource.VerifierIdentity || resource.VerifierIdentity == "" || resource.UnavailableReason != "" {
			return "", ErrDependency
		}
	case ProviderUnavailable:
		if resource.Verifier != nil || resource.VerifierIdentity != "" || !slices.Contains([]string{"discovery", "keys", "credentials", "host-policy"}, resource.UnavailableReason) {
			return "", ErrDependency
		}
	default:
		return "", ErrDependency
	}
	return hash(struct {
		Source                  ProviderDescriptor
		Availability            ProviderAvailability
		Verifier, Trust, Reason string
	}{source, resource.Availability, resource.VerifierIdentity, resource.TrustIdentity, resource.UnavailableReason}), nil
}
func (v *View) prepareProviders(ctx context.Context, deps Dependencies) error {
	if deps.ProviderResources == nil {
		return ErrManifest
	}
	if len(deps.ProviderResources) > MaxProviders {
		return ErrBounds
	}
	sources, err := ProviderSources(ctx, v.cut, deps.ConfigProviders)
	if err != nil {
		return err
	}
	if len(sources) != len(deps.ProviderResources) {
		return ErrDependency
	}
	resources := map[string]ProviderResource{}
	for _, resource := range deps.ProviderResources {
		if len(resource.Ref.ID) > 63 || len(resource.Ref.UID) > 512 || len(resource.Ref.Origin) > 6 {
			return ErrBounds
		}
		if _, ok := resources[resource.Ref.ID]; ok {
			return ErrDependency
		}
		if len(resource.SpecIdentity) > 64 || len(resource.PolicyIdentity) > 64 || len(resource.VerifierIdentity) > 64 || len(resource.UnavailableReason) > 32 || len(resource.TrustIdentity) > 512 {
			return ErrBounds
		}
		resources[resource.Ref.ID] = resource
	}
	v.providerViews = map[string]compiledProvider{}
	identities := []string{}
	for _, source := range sources {
		resource, ok := resources[source.Ref.ID]
		if !ok {
			return ErrDependency
		}
		identity, err := ProviderResourceIdentity(source, resource)
		if err != nil {
			return err
		}
		policy, err := oidccore.Compile(source.Spec, source.Ref.ID)
		if err != nil || policy.Identity() != source.PolicyIdentity {
			return ErrDependency
		}
		if role := source.Spec.Provisioning.DefaultRole; role != "" {
			if role == "admin" || slices.Contains(v.permissions.Scoped, role) {
				return ErrInvalid
			}
			if _, ok := v.permissions.Roles[role]; !ok {
				return ErrInvalid
			}
		}
		v.providerViews[source.Ref.ID] = compiledProvider{source, resource, identity, policy}
		identities = append(identities, identity)
	}
	v.providerIdentity = hash(identities)
	return nil
}

// ProviderInfo is safe detached metadata for host flow pinning, with no verifier/secrets.
type ProviderInfo struct {
	Descriptor       ProviderDescriptor
	Availability     ProviderAvailability
	ResourceIdentity string
}

// ProviderInfo returns a detached captured descriptor for flow pinning.
func (v *View) ProviderInfo(id string) (ProviderInfo, error) {
	if v == nil || v.released.Load() {
		return ProviderInfo{}, ErrInvalid
	}
	p, ok := v.providerViews[id]
	if !ok {
		return ProviderInfo{}, ErrCredentials
	}
	raw, _ := json.Marshal(p.descriptor)
	var descriptor ProviderDescriptor
	_ = json.Unmarshal(raw, &descriptor)
	return ProviderInfo{descriptor, p.resource.Availability, p.identity}, nil
}
