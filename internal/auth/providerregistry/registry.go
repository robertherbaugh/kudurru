package providerregistry

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	publicv1 "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/auth/oidccore"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"k8s.io/apimachinery/pkg/util/validation"
)

var uidPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)
var hashPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

// SourceKey identifies a retained config slot, never a credential.
func SourceKey(id string) string { return versionPrefix + "config/" + id }

// AuthorityKey binds a receipt to an exact source origin and incarnation.
func AuthorityKey(ref servingauth.ProviderRef) string {
	return versionPrefix + "authority/" + digest(ref)
}

// Empty constructs an explicit detached migration input, never a read fallback.
func Empty(clusterID string) (*Registry, error) {
	if !text(clusterID, 256, false) {
		return nil, ErrInvalid
	}
	r := &Registry{root: Root{FormatVersion: FormatVersion, ClusterID: clusterID, Epoch: 1, Sources: []string{}, Authorities: []string{}}, sources: []Source{}, authorities: []AuthorityRecord{}}
	r.digest = digest(rows(r))
	return r, nil
}

// Digest returns the complete canonical registry identity.
func (r *Registry) Digest() string {
	if r == nil {
		return ""
	}
	return r.digest
}

// Epoch returns the persistent monotonic registry epoch.
func (r *Registry) Epoch() uint64 {
	if r == nil {
		return 0
	}
	return r.root.Epoch
}

// Sources returns detached retained config records, including tombstones.
func (r *Registry) Sources() []Source {
	if r == nil {
		return nil
	}
	out := make([]Source, len(r.sources))
	for i, s := range r.sources {
		out[i] = s
		out[i].Spec = cloneSpec(s.Spec)
	}
	return out
}

// Authorities returns detached accepted receipts for both origins.
func (r *Registry) Authorities() []AuthorityRecord {
	if r == nil {
		return nil
	}
	return slices.Clone(r.authorities)
}

// ConfigSources projects canonical config precedence without granting capabilities.
func (r *Registry) ConfigSources() []servingauth.ProviderSource {
	if r == nil {
		return nil
	}
	out := []servingauth.ProviderSource{}
	for _, s := range r.sources {
		if s.Active {
			out = append(out, servingauth.ProviderSource{Ref: servingauth.ProviderRef{ID: s.ID, UID: s.UID, Origin: servingauth.ProviderConfig}, CreatedAt: s.CreatedAt, Spec: cloneSpec(s.Spec)})
		}
	}
	return out
}

// Encode produces deterministic rows from an opaque validated registry.
func Encode(r *Registry) ([]replication.Entry, error) {
	if r == nil || r.digest == "" {
		return nil, ErrInvalid
	}
	out := rows(r)
	if _, err := Decode(out, r.root.ClusterID); err != nil {
		return nil, err
	}
	return out, nil
}

func rows(r *Registry) []replication.Entry {
	root := r.root
	root.Sources = []string{}
	root.Authorities = []string{}
	out := make([]replication.Entry, 0, 1+len(r.sources)+len(r.authorities))
	add := func(key string, v any) {
		data, _ := json.Marshal(v)
		out = append(out, replication.Entry{Key: key, Data: data})
	}
	for _, s := range r.sources {
		key := SourceKey(s.ID)
		root.Sources = append(root.Sources, key)
		add(key, s)
	}
	for _, a := range r.authorities {
		key := AuthorityKey(a.Provider)
		root.Authorities = append(root.Authorities, key)
		add(key, a)
	}
	slices.Sort(root.Sources)
	slices.Sort(root.Authorities)
	add(RootKey, root)
	slices.SortFunc(out, func(a, b replication.Entry) int { return strings.Compare(a.Key, b.Key) })
	return out
}

// Decode requires one exact complete manifest; other owners remain independent.
func Decode(entries []replication.Entry, clusterID string) (*Registry, error) {
	if !text(clusterID, 256, false) {
		return nil, ErrInvalid
	}
	if len(entries) > servingauth.MaxRows {
		return nil, ErrBounds
	}
	total, ownBytes, ownCount := 0, 0, 0
	for _, e := range entries {
		total += len(e.Data)
		if len(e.Key) > 512 || total > servingauth.MaxSecurityBytes {
			return nil, ErrBounds
		}
		if strings.HasPrefix(e.Key, prefix) {
			ownCount++
			ownBytes += len(e.Data) + len(e.Key)
			limit := MaxRowBytes
			if e.Key == RootKey {
				limit = MaxRootBytes
			}
			if len(e.Data) > limit || ownBytes > MaxBytes || ownCount > MaxRows {
				return nil, ErrBounds
			}
		}
	}
	own := make(map[string]replication.Entry, ownCount)
	for _, e := range entries {
		if !strings.HasPrefix(e.Key, prefix) {
			continue
		}
		if !strings.HasPrefix(e.Key, versionPrefix) {
			return nil, ErrVersion
		}
		if _, ok := own[e.Key]; ok {
			return nil, ErrInvalid
		}
		own[e.Key] = e
	}
	entry, ok := own[RootKey]
	if !ok {
		return nil, ErrManifest
	}
	var root Root
	if err := strict(entry.Data, &root, MaxRootBytes); err != nil {
		return nil, err
	}
	if root.FormatVersion != FormatVersion {
		return nil, ErrVersion
	}
	if root.ClusterID != clusterID || root.Epoch == 0 {
		return nil, ErrInvalid
	}
	if root.Sources == nil || root.Authorities == nil {
		return nil, ErrManifest
	}
	if len(root.Sources) > MaxSources || len(root.Authorities) > MaxAuthorities {
		return nil, ErrBounds
	}
	if len(own) != 1+len(root.Sources)+len(root.Authorities) {
		return nil, ErrManifest
	}
	r := &Registry{root: root, sources: []Source{}, authorities: []AuthorityRecord{}}
	seen := map[string]bool{RootKey: true}
	uids := map[string]bool{}
	read := func(key string, dst any) error {
		e, ok := own[key]
		if !ok || seen[key] {
			return ErrManifest
		}
		seen[key] = true
		return strict(e.Data, dst, MaxRowBytes)
	}
	active := 0
	for _, key := range root.Sources {
		var s Source
		if err := read(key, &s); err != nil {
			return nil, err
		}
		if err := validSource(s); err != nil {
			return nil, err
		}
		if key != SourceKey(s.ID) || uids[s.UID] || s.Revision > root.Epoch {
			return nil, ErrInvalid
		}
		uids[s.UID] = true
		if s.Active {
			active++
		}
		r.sources = append(r.sources, s)
	}
	if active > MaxActiveSources {
		return nil, ErrBounds
	}
	active = 0
	authorities := map[string]AuthorityRecord{}
	activeIDs := map[string]bool{}
	for _, key := range root.Authorities {
		var a AuthorityRecord
		if err := read(key, &a); err != nil {
			return nil, err
		}
		if err := validAuthority(a); err != nil {
			return nil, err
		}
		if key != AuthorityKey(a.Provider) || a.Revision > root.Epoch {
			return nil, ErrInvalid
		}
		if a.Active {
			if activeIDs[a.Provider.ID] {
				return nil, ErrInvalid
			}
			activeIDs[a.Provider.ID] = true
			active++
		}
		authorities[key] = a
		r.authorities = append(r.authorities, a)
	}
	if active > MaxActiveAuthorities {
		return nil, ErrBounds
	}
	for _, s := range r.sources {
		if s.AuthorityKey == "" {
			continue
		}
		a, ok := authorities[s.AuthorityKey]
		if !s.Active || !ok || !a.Active || a.Provider != (servingauth.ProviderRef{ID: s.ID, UID: s.UID, Origin: servingauth.ProviderConfig}) {
			return nil, ErrInvalid
		}
		d, err := configDescriptor(s)
		if err != nil || d.SpecIdentity != a.SpecIdentity {
			return nil, ErrInvalid
		}
	}
	for _, a := range r.authorities {
		if !a.Active {
			continue
		}
		if a.Provider.Origin == servingauth.ProviderConfig {
			found := false
			for _, s := range r.sources {
				if s.Active && s.AuthorityKey == AuthorityKey(a.Provider) {
					found = true
				}
			}
			if !found {
				return nil, ErrInvalid
			}
		} else {
			for _, s := range r.sources {
				if s.Active && s.ID == a.Provider.ID {
					return nil, ErrInvalid
				}
			}
		}
	}
	slices.SortFunc(r.sources, func(a, b Source) int { return strings.Compare(a.ID, b.ID) })
	slices.SortFunc(r.authorities, func(a, b AuthorityRecord) int {
		return strings.Compare(AuthorityKey(a.Provider), AuthorityKey(b.Provider))
	})
	r.digest = digest(rows(r))
	return r, nil
}

// ValidateEntry validates only this owner; root composes complete image validation.
func ValidateEntry(section string, e replication.Entry) error {
	if section != "security" || !strings.HasPrefix(e.Key, prefix) {
		return nil
	}
	if !strings.HasPrefix(e.Key, versionPrefix) {
		return ErrVersion
	}
	if len(e.Key) > 512 {
		return ErrBounds
	}
	switch {
	case e.Key == RootKey:
		var r Root
		if err := strict(e.Data, &r, MaxRootBytes); err != nil {
			return err
		}
		if r.FormatVersion != FormatVersion {
			return ErrVersion
		}
		if !text(r.ClusterID, 256, false) || r.Epoch == 0 || r.Sources == nil || r.Authorities == nil {
			return ErrManifest
		}
		if len(r.Sources) > MaxSources || len(r.Authorities) > MaxAuthorities {
			return ErrBounds
		}
		seen := map[string]bool{}
		for _, keys := range [][]string{r.Sources, r.Authorities} {
			for _, key := range keys {
				if seen[key] || len(key) > 512 || !strings.HasPrefix(key, versionPrefix) || key == RootKey {
					return ErrInvalid
				}
				seen[key] = true
			}
		}
		return nil
	case strings.HasPrefix(e.Key, versionPrefix+"config/"):
		var s Source
		if err := strict(e.Data, &s, MaxRowBytes); err != nil {
			return err
		}
		if err := validSource(s); err != nil {
			return err
		}
		if e.Key != SourceKey(s.ID) {
			return ErrInvalid
		}
		return nil
	case strings.HasPrefix(e.Key, versionPrefix+"authority/"):
		var a AuthorityRecord
		if err := strict(e.Data, &a, MaxRowBytes); err != nil {
			return err
		}
		if err := validAuthority(a); err != nil {
			return err
		}
		if e.Key != AuthorityKey(a.Provider) {
			return ErrInvalid
		}
		return nil
	default:
		return ErrVersion
	}
}

// ValidateImage validates the complete registry against the actual image cluster.
func ValidateImage(image *replication.Image) error {
	if image == nil || !image.Complete {
		return ErrManifest
	}
	_, err := Decode(image.Payload.Security, image.Term.ClusterID)
	return err
}

func validSource(s Source) error {
	if s.FormatVersion != FormatVersion {
		return ErrVersion
	}
	if !validID(s.ID) || !uidPattern.MatchString(s.UID) || !validTime(s.CreatedAt) || s.Revision == 0 || !hashPattern.MatchString(s.SpecDigest) || s.SpecDigest != specDigest(s.Spec) || s.AuthorityKey != "" && !strings.HasPrefix(s.AuthorityKey, versionPrefix+"authority/") {
		return ErrInvalid
	}
	_, err := oidccore.Compile(s.Spec, s.ID)
	if err != nil {
		return ErrInvalid
	}
	return nil
}
func validAuthority(a AuthorityRecord) error {
	if a.FormatVersion != FormatVersion {
		return ErrVersion
	}
	if !validRef(a.Provider) || !hashPattern.MatchString(a.SpecIdentity) || a.Revision == 0 {
		return ErrInvalid
	}
	for _, s := range []string{a.Binding.Profile, a.Binding.FingerprintKeyID, a.Binding.CredentialFingerprint, a.Binding.CredentialPolicy, a.Binding.NetworkPolicy, a.Binding.MetadataBinding, a.Binding.EntitlementIdentity} {
		if !text(s, 512, false) {
			return ErrInvalid
		}
	}
	return nil
}
func validID(id string) bool {
	return len(id) <= 63 && len(validation.IsDNS1123Label(id)) == 0 && !slices.Contains([]string{"local", "kudurru", "unix", "internal", "kubernetes"}, id)
}
func validRef(p servingauth.ProviderRef) bool {
	return validID(p.ID) && text(p.UID, 512, false) && (p.Origin == servingauth.ProviderObject || p.Origin == servingauth.ProviderConfig && uidPattern.MatchString(p.UID))
}
func text(s string, limit int, empty bool) bool {
	if len(s) > limit || !empty && s == "" || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
func validTime(t time.Time) bool { return !t.IsZero() && t.Year() >= 1970 && t.Year() <= 9999 }
func digest(v any) string {
	raw, _ := json.Marshal(v)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func specDigest(s publicv1.IdentityProviderSpec) string {
	return digest(struct {
		Profile string
		Spec    publicv1.IdentityProviderSpec
	}{"providerregistry/spec/v1", s})
}
func cloneSpec(s publicv1.IdentityProviderSpec) publicv1.IdentityProviderSpec {
	raw, _ := json.Marshal(s)
	var out publicv1.IdentityProviderSpec
	_ = json.Unmarshal(raw, &out)
	return out
}

func configDescriptor(s Source) (servingauth.ProviderDescriptor, error) {
	p, err := oidccore.Compile(s.Spec, s.ID)
	if err != nil {
		return servingauth.ProviderDescriptor{}, ErrInvalid
	}
	ref := servingauth.ProviderRef{ID: s.ID, UID: s.UID, Origin: servingauth.ProviderConfig}
	identity := digest(struct {
		Profile   string
		Ref       servingauth.ProviderRef
		CreatedAt time.Time
		Spec      publicv1.IdentityProviderSpec
	}{"servingauth/provider/v1", ref, s.CreatedAt.UTC(), s.Spec})
	return servingauth.ProviderDescriptor{Ref: ref, CreatedAt: s.CreatedAt.UTC(), Issuer: s.Spec.Issuer, SpecIdentity: identity, PolicyIdentity: p.Identity(), Spec: p.Spec()}, nil
}
