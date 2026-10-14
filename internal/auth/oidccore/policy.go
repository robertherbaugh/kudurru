package oidccore

import (
	"context"
	"encoding/json"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	publicv1 "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"k8s.io/apimachinery/pkg/util/validation"
)

// Identity preserves the existing provider/issuer/subject identity semantics.
// It carries no durable UID, builtin-admin authority or authorization verdict.
type Identity struct {
	Provider, Issuer, Subject                   string
	Username, UsernameClaim, DisplayName, Email string
	Groups                                      []string
}

// ID renders the established query-escaped external identity tuple.
func (i Identity) ID() string { return (IdentityKey{i.Provider, i.Issuer, i.Subject}).String() }

// IdentityKey is never based on display name or email.
type IdentityKey struct{ Provider, Issuer, Subject string }

func (k IdentityKey) String() string {
	return "oidc:" + url.QueryEscape(k.Provider) + ":" + url.QueryEscape(k.Issuer) + ":" + url.QueryEscape(k.Subject)
}

// ParseIdentityKey accepts the bounded canonical output of IdentityKey.String.
func ParseIdentityKey(raw string) (IdentityKey, error) {
	if len(raw) > 3*(63+MaxIssuerBytes+MaxSubjectBytes)+8 {
		return IdentityKey{}, ErrBounds
	}
	s, ok := strings.CutPrefix(raw, "oidc:")
	if !ok {
		return IdentityKey{}, ErrInvalid
	}
	a := strings.SplitN(s, ":", 4)
	if len(a) != 3 {
		return IdentityKey{}, ErrInvalid
	}
	k := IdentityKey{}
	for i, p := range []*string{&k.Provider, &k.Issuer, &k.Subject} {
		v, err := url.QueryUnescape(a[i])
		if err != nil || !validText(v, []int{63, MaxIssuerBytes, MaxSubjectBytes}[i]) {
			return IdentityKey{}, ErrInvalid
		}
		*p = v
	}
	if k.String() != raw {
		return IdentityKey{}, ErrInvalid
	}
	return k, nil
}

// Policy contains detached validated defaults and checked CEL programs only.
// It cannot resolve credentials, provision users or decide management rights.
type Policy struct {
	id, identity                     string
	spec                             publicv1.IdentityProviderSpec
	audiences                        []string
	rules                            []*expression
	username, groups, display, email *expression
}

func (*Policy) String() string { return "oidccore: policy" }

// GoString omits policy documents from generic diagnostics.
func (p *Policy) GoString() string { return p.String() }

// MarshalJSON omits policy documents from generic diagnostics.
func (*Policy) MarshalJSON() ([]byte, error) { return []byte(`"policy"`), nil }

// Identity pins the exact normalized policy and compiler profile.
func (p *Policy) Identity() string {
	if p == nil {
		return ""
	}
	return p.identity
}

// Spec is a detached normalized policy projection. Credential references do not
// become host capabilities and no resolved credential is ever stored here.
func (p *Policy) Spec() publicv1.IdentityProviderSpec {
	if p == nil {
		return publicv1.IdentityProviderSpec{}
	}
	raw, _ := json.Marshal(p.spec)
	var out publicv1.IdentityProviderSpec
	_ = json.Unmarshal(raw, &out)
	return out
}

// Compile validates all bounded public policy fields before cloning/compiling.
// Role existence/scoped-default checks belong to the root's permissions owner.
func Compile(spec publicv1.IdentityProviderSpec, providerID string) (*Policy, error) {
	if len(validation.IsDNS1123Label(providerID)) != 0 || !validURL(spec.Issuer, false) || !validText(spec.ClientID, 256) || spec.RedirectURL != "" && !validURL(spec.RedirectURL, true) {
		return nil, ErrPolicy
	}
	if err := boundedSpec(spec); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		return nil, ErrPolicy
	}
	if len(raw) > MaxPolicyBytes {
		return nil, ErrBounds
	}
	p := &Policy{id: providerID}
	if json.Unmarshal(raw, &p.spec) != nil {
		return nil, ErrPolicy
	}
	s := &p.spec
	if s.DisplayName == "" {
		s.DisplayName = providerID
	}
	if s.ClaimMappings.Username.Prefix == nil {
		v := providerID + ":"
		s.ClaimMappings.Username.Prefix = &v
	}
	if s.ClaimMappings.Groups.Prefix == nil {
		v := providerID + ":"
		s.ClaimMappings.Groups.Prefix = &v
	}
	if s.ClaimMappings.Groups.Claim == "" && s.ClaimMappings.Groups.Expression == "" {
		s.ClaimMappings.Groups.Claim = "groups"
	}
	if s.ClaimMappings.DisplayName.Claim == "" && s.ClaimMappings.DisplayName.Expression == "" {
		s.ClaimMappings.DisplayName.Claim = "name"
	}
	if s.ClaimMappings.Email.Claim == "" && s.ClaimMappings.Email.Expression == "" {
		s.ClaimMappings.Email.Claim = "email"
	}
	if len(s.Scopes) == 0 {
		s.Scopes = []string{"openid", "profile", "email", "groups"}
	}
	if !slices.Contains(s.Scopes, "openid") {
		if len(s.Scopes) == 64 {
			return nil, ErrBounds
		}
		s.Scopes = append([]string{"openid"}, s.Scopes...)
	}
	switch s.Provisioning.AutoProvision {
	case "":
		s.Provisioning.AutoProvision = "mapped-only"
	case "mapped-only", "always", "never":
	default:
		return nil, ErrPolicy
	}
	if s.Provisioning.DefaultRole == "admin" || s.Provisioning.DefaultRole != "" && (s.Provisioning.AutoProvision == "mapped-only" || !validText(s.Provisioning.DefaultRole, 63)) {
		return nil, ErrPolicy
	}
	for _, a := range append([]string{s.ClientID, s.CLIClientID}, s.Audiences...) {
		if a != "" && !slices.Contains(p.audiences, a) {
			p.audiences = append(p.audiences, a)
		}
	}
	slices.Sort(p.audiences)
	for i, d := range s.AllowedDomains {
		d = strings.ToLower(d)
		if len(validation.IsDNS1123Subdomain(d)) != 0 {
			return nil, ErrPolicy
		}
		s.AllowedDomains[i] = d
	}
	slices.Sort(s.AllowedDomains)
	s.AllowedDomains = slices.Compact(s.AllowedDomains)
	b := expressionBuilder{emailClaim: s.ClaimMappings.Email.Claim}
	for _, rule := range s.ClaimValidationRules {
		if (rule.Claim == "") == (rule.Expression == "") || rule.Expression != "" && rule.RequiredValue != "" {
			return nil, ErrPolicy
		}
		e, err := b.compile(rule.Expression, boolResult)
		if err != nil {
			return nil, err
		}
		p.rules = append(p.rules, e)
	}
	for _, mapping := range []struct {
		claim, source string
		kind          resultKind
		target        **expression
	}{
		{s.ClaimMappings.Username.Claim, s.ClaimMappings.Username.Expression, stringResult, &p.username},
		{s.ClaimMappings.Groups.Claim, s.ClaimMappings.Groups.Expression, groupsResult, &p.groups},
		{s.ClaimMappings.DisplayName.Claim, s.ClaimMappings.DisplayName.Expression, stringResult, &p.display},
		{s.ClaimMappings.Email.Claim, s.ClaimMappings.Email.Expression, stringResult, &p.email},
	} {
		if mapping.claim != "" && mapping.source != "" {
			return nil, ErrPolicy
		}
		e, err := b.compile(mapping.source, mapping.kind)
		if err != nil {
			return nil, err
		}
		*mapping.target = e
	}
	p.identity = digest(struct {
		Profile, Provider string
		Spec              publicv1.IdentityProviderSpec
	}{"oidccore/policy/v1-cel0.29.2", providerID, p.spec})
	return p, nil
}

// Evaluate requires verified facts, current supplied time, captured issuer and
// audience policy. Browser mode requires VerifyBrowser's nonce/hash proof.
func (p *Policy) Evaluate(ctx context.Context, token *VerifiedToken, mode ProofMode, now time.Time) (Identity, error) {
	if err := ctx.Err(); err != nil {
		return Identity{}, err
	}
	if p == nil || p.identity == "" || token == nil || !token.verified || token.facts.Issuer != p.spec.Issuer || token.facts.VerifierIdentity == "" {
		return Identity{}, ErrPolicy
	}
	if err := token.ValidAt(now); err != nil {
		return Identity{}, err
	}
	return p.evaluateClaims(ctx, token, mode)
}

// evaluateClaims is shared only with authenticated evidence; raw Evaluate always
// performs the strict verification marker and current time checks above.
func (p *Policy) evaluateClaims(ctx context.Context, token *VerifiedToken, mode ProofMode) (Identity, error) {
	accepted := p.audiences
	switch mode {
	case BearerIDToken:
	case BrowserIDToken:
		if !token.browser {
			return Identity{}, ErrBrowser
		}
		accepted = []string{p.spec.ClientID}
	default:
		return Identity{}, ErrPolicy
	}
	hit := false
	for _, a := range token.facts.Audiences {
		hit = hit || slices.Contains(accepted, a)
	}
	azp, hasAzp := token.claims["azp"].(string)
	if !hit || len(token.facts.Audiences) > 1 && !hasAzp || hasAzp && !slices.Contains(accepted, azp) {
		return Identity{}, ErrAudience
	}
	ev := evaluation{ctx: ctx, claims: token.claims}
	for i, r := range p.spec.ClaimValidationRules {
		if p.rules[i] == nil {
			if !scalarEquals(token.claims[r.Claim], r.RequiredValue) {
				return Identity{}, ErrPolicy
			}
		} else {
			value, err := ev.run(p.rules[i])
			if err != nil {
				return Identity{}, err
			}
			if value != true {
				return Identity{}, ErrPolicy
			}
		}
	}
	verifiedEmail := token.claims["email_verified"] == true || token.claims["email_verified"] == "true"
	id := Identity{Provider: p.id, Issuer: token.facts.Issuer, Subject: token.facts.Subject}
	var err error
	id.DisplayName, err = p.mappedString(&ev, p.display, p.spec.ClaimMappings.DisplayName.Claim)
	if err != nil {
		return Identity{}, err
	}
	if verifiedEmail {
		id.Email, err = p.mappedString(&ev, p.email, p.spec.ClaimMappings.Email.Claim)
		if err != nil {
			return Identity{}, err
		}
	} else if p.email != nil {
		// Evaluate expressions even for unverified email to reject type/errors,
		// then discard their output rather than allowing a verification bypass.
		if _, err = ev.run(p.email); err != nil {
			return Identity{}, err
		}
	}
	if len(p.spec.AllowedDomains) > 0 && (!verifiedEmail || !allowedEmail(id.Email, p.spec.AllowedDomains)) {
		return Identity{}, ErrPolicy
	}
	if p.username != nil {
		if p.username.usesEmail && !verifiedEmail {
			return Identity{}, ErrPolicy
		}
		value, err := ev.run(p.username)
		if err != nil {
			return Identity{}, err
		}
		name := value.(string)
		// CEL may choose arbitrary claim paths; do not admit unverified email
		// as a username when the expression's result is an email address.
		if !validText(name, 256) || strings.Contains(name, "@") && !verifiedEmail {
			return Identity{}, ErrPolicy
		}
		id.Username, id.UsernameClaim = name, "expression"
	} else {
		candidates := []string{p.spec.ClaimMappings.Username.Claim, "preferred_username", "upn", "email", "sub"}
		for _, claim := range candidates {
			if claim == "" || !verifiedEmail && (claim == "email" || claim == p.spec.ClaimMappings.Email.Claim) {
				continue
			}
			name, _ := token.claims[claim].(string)
			if validText(name, 256) {
				id.Username, id.UsernameClaim = name, claim
				break
			}
		}
	}
	id.Username = *p.spec.ClaimMappings.Username.Prefix + id.Username
	if id.UsernameClaim == "" || !validText(id.Username, MaxNameBytes) {
		return Identity{}, ErrPolicy
	}
	if err := distributedGroups(token.claims, p.spec.ClaimMappings.Groups.Claim, p.groups != nil); err != nil {
		return Identity{}, err
	}
	var groups []string
	if p.groups != nil {
		value, err := ev.run(p.groups)
		if err != nil {
			return Identity{}, err
		}
		groups = value.([]string)
	} else {
		groups = scalarGroups(token.claims[p.spec.ClaimMappings.Groups.Claim])
	}
	if p.spec.ClaimMappings.Groups.Claim != "roles" || p.groups != nil {
		groups = append(groups, scalarGroups(token.claims["roles"])...)
	}
	seen := map[string]bool{}
	for _, g := range groups {
		g = *p.spec.ClaimMappings.Groups.Prefix + g
		if !validText(g, MaxNameBytes) {
			return Identity{}, ErrPolicy
		}
		if !seen[g] {
			if len(id.Groups) >= MaxGroups {
				return Identity{}, ErrBounds
			}
			id.Groups = append(id.Groups, g)
			seen[g] = true
		}
	}
	slices.Sort(id.Groups)
	if err = ctx.Err(); err != nil {
		return Identity{}, err
	}
	return id, nil
}

func (p *Policy) mappedString(ev *evaluation, expr *expression, claim string) (string, error) {
	if expr != nil {
		value, err := ev.run(expr)
		if err != nil {
			return "", err
		}
		return value.(string), nil
	}
	s, _ := ev.claims[claim].(string)
	if len(s) > MaxNameBytes || !validOptionalText(s, MaxNameBytes) {
		return "", ErrPolicy
	}
	return s, nil
}

func scalarEquals(value any, want string) bool {
	switch v := value.(type) {
	case string:
		return v == want
	case bool:
		return strconv.FormatBool(v) == want
	case json.Number:
		return string(v) == want
	}
	return false
}

func scalarGroups(value any) []string {
	var out []string
	switch v := value.(type) {
	case string:
		if v != "" {
			out = append(out, v)
		}
	case []any:
		for _, e := range v {
			if s, ok := e.(string); ok && s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

func distributedGroups(claims map[string]any, claim string, expression bool) error {
	raw, exists := claims["_claim_names"]
	if !exists {
		return nil
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return ErrPolicy
	}
	for name := range m {
		if (name == claim || expression || name == "roles") && claims[name] == nil {
			return ErrPolicy
		}
	}
	return nil
}

func allowedEmail(email string, domains []string) bool {
	if strings.Count(email, "@") != 1 {
		return false
	}
	local, domain, _ := strings.Cut(email, "@")
	return local != "" && slices.Contains(domains, strings.ToLower(domain))
}

func validURL(raw string, redirect bool) bool {
	if !validText(raw, MaxIssuerBytes) {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || u.Fragment != "" || !redirect && u.RawQuery != "" {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	return u.Scheme == "http" && (strings.EqualFold(u.Hostname(), "localhost") || net.ParseIP(u.Hostname()).IsLoopback())
}

func validText(s string, limit int) bool { return s != "" && validOptionalText(s, limit) }
func validOptionalText(s string, limit int) bool {
	return len(s) <= limit && utf8.ValidString(s) && !strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f })
}

func boundedSpec(s publicv1.IdentityProviderSpec) error {
	if len(s.Scopes) > 64 || len(s.Audiences) > 64 || len(s.ClaimValidationRules) > 64 || len(s.AllowedDomains) > 64 {
		return ErrBounds
	}
	for _, value := range []string{s.DisplayName, s.CLIClientID, s.Provisioning.DefaultRole} {
		if !validOptionalText(value, 256) {
			return ErrPolicy
		}
	}
	for _, values := range [][]string{s.Scopes, s.Audiences, s.AllowedDomains} {
		for _, v := range values {
			if !validText(v, 256) {
				return ErrPolicy
			}
		}
	}
	for _, r := range s.ClaimValidationRules {
		if !validOptionalText(r.Claim, 256) || len(r.RequiredValue) > 2048 || !utf8.ValidString(r.RequiredValue) || len(r.Expression) > 4096 || !utf8.ValidString(r.Expression) {
			return ErrBounds
		}
	}
	for _, m := range []publicv1.PrefixedClaim{s.ClaimMappings.Username, s.ClaimMappings.Groups} {
		if !validOptionalText(m.Claim, 256) || len(m.Expression) > 4096 || !utf8.ValidString(m.Expression) || m.Prefix != nil && !validOptionalText(*m.Prefix, 256) {
			return ErrBounds
		}
	}
	for _, m := range []publicv1.ClaimRef{s.ClaimMappings.DisplayName, s.ClaimMappings.Email} {
		if !validOptionalText(m.Claim, 256) || len(m.Expression) > 4096 || !utf8.ValidString(m.Expression) {
			return ErrBounds
		}
	}
	if r := s.ClientSecretRef; r != nil {
		if !validOptionalText(r.File, 4096) || !validOptionalText(r.Env, 128) || r.SecretRef != nil && (!validText(r.SecretRef.Name, 253) || !validText(r.SecretRef.Key, 253)) {
			return ErrBounds
		}
		count := 0
		if r.File != "" {
			count++
		}
		if r.Env != "" {
			count++
		}
		if r.SecretRef != nil {
			count++
		}
		if count != 1 {
			return ErrPolicy
		}
	}
	// Reserve bounded structural overhead (including defaults and all 64 rule
	// field names) before JSON cloning. Escape expansion is counted explicitly;
	// json.Marshal must not allocate a large document only to reject it afterward.
	budget := 4096
	consume := func(value string) bool { budget += jsonStringBytes(value); return budget <= MaxPolicyBytes }
	for _, value := range []string{s.DisplayName, s.Issuer, s.ClientID, s.CLIClientID, s.RedirectURL, s.Provisioning.AutoProvision, s.Provisioning.DefaultRole} {
		if !consume(value) {
			return ErrBounds
		}
	}
	for _, values := range [][]string{s.Scopes, s.Audiences, s.AllowedDomains} {
		for _, value := range values {
			if !consume(value) {
				return ErrBounds
			}
		}
	}
	for _, r := range s.ClaimValidationRules {
		for _, value := range []string{r.Claim, r.RequiredValue, r.Expression} {
			if !consume(value) {
				return ErrBounds
			}
		}
	}
	for _, m := range []publicv1.PrefixedClaim{s.ClaimMappings.Username, s.ClaimMappings.Groups} {
		if !consume(m.Claim) || !consume(m.Expression) {
			return ErrBounds
		}
		if m.Prefix != nil && !consume(*m.Prefix) {
			return ErrBounds
		}
	}
	for _, m := range []publicv1.ClaimRef{s.ClaimMappings.DisplayName, s.ClaimMappings.Email} {
		if !consume(m.Claim) || !consume(m.Expression) {
			return ErrBounds
		}
	}
	if r := s.ClientSecretRef; r != nil {
		if !consume(r.File) || !consume(r.Env) {
			return ErrBounds
		}
		if r.SecretRef != nil && (!consume(r.SecretRef.Name) || !consume(r.SecretRef.Key)) {
			return ErrBounds
		}
	}
	return nil
}

func jsonStringBytes(value string) int {
	count := 2
	for _, r := range value {
		switch r {
		case '"', '\\', '\b', '\f', '\n', '\r', '\t':
			count += 2
		case '<', '>', '&', '\u2028', '\u2029':
			count += 6
		default:
			if r < 0x20 {
				count += 6
			} else {
				count += utf8.RuneLen(r)
			}
		}
	}
	return count
}
