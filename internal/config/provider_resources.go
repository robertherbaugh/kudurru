package config

import (
	"errors"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// ProviderResources grants host-only OIDC acquisition capabilities. Public
// IdentityProvider objects and discovery documents cannot extend these grants.
// TLS material is loaded once before resource publication, never during auth.
type ProviderResources struct {
	AllowedOrigins []string `koanf:"allowedOrigins" json:"allowedOrigins"`
	LocalOrigins   []string `koanf:"localOrigins" json:"localOrigins"`
	Algorithms     []string `koanf:"algorithms" json:"algorithms"`
	SystemTrust    bool     `koanf:"systemTrust" json:"systemTrust"`
	CAFiles        []string `koanf:"caFiles" json:"caFiles"`
}

func (p ProviderResources) validate() []error {
	bad := func(message string) []error { return []error{errors.New("auth.providerResources: " + message)} }
	if len(p.AllowedOrigins) > 128 || len(p.LocalOrigins) > 128 || len(p.Algorithms) == 0 || len(p.Algorithms) > 10 || len(p.CAFiles) > 32 {
		return bad("at most 128 origins, 1 to 10 algorithms and 32 CA files are allowed")
	}
	allowed := make(map[string]bool, len(p.AllowedOrigins))
	for _, raw := range p.AllowedOrigins {
		if !validProviderOrigin(raw) || allowed[raw] {
			return bad("origins must be unique lowercase HTTP(S) origins without paths, credentials, queries or fragments")
		}
		allowed[raw] = true
	}
	local := make(map[string]bool, len(p.LocalOrigins))
	for _, raw := range p.LocalOrigins {
		if !allowed[raw] || local[raw] {
			return bad("local origins must be a unique subset of allowed origins")
		}
		local[raw] = true
	}
	for _, raw := range p.AllowedOrigins {
		if strings.HasPrefix(raw, "http://") && !local[raw] {
			return bad("HTTP origins require an explicit local exception and only permit loopback connections")
		}
	}
	seen := map[string]bool{}
	for _, algorithm := range p.Algorithms {
		if !slices.Contains([]string{"RS256", "RS384", "RS512", "PS256", "PS384", "PS512", "ES256", "ES384", "ES512", "EdDSA"}, algorithm) || seen[algorithm] {
			return bad("algorithms must be unique supported asymmetric JWT algorithms")
		}
		seen[algorithm] = true
	}
	seen = map[string]bool{}
	for _, file := range p.CAFiles {
		if len(file) > 4096 || strings.ContainsRune(file, 0) || !filepath.IsAbs(file) || filepath.Clean(file) != file || seen[file] {
			return bad("CA files must be unique clean absolute paths of at most 4096 bytes")
		}
		seen[file] = true
	}
	if len(p.AllowedOrigins) > 0 && !p.SystemTrust && len(p.CAFiles) == 0 {
		return bad("allowed origins require system trust or explicit CA files")
	}
	return nil
}

func validProviderOrigin(raw string) bool {
	if len(raw) == 0 || len(raw) > 2048 {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.Host != strings.ToLower(u.Host) || strings.Contains(u.Host, "%") || u.User != nil || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || raw != u.Scheme+"://"+u.Host {
		return false
	}
	if strings.HasSuffix(u.Host, ":") {
		return false
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
			return false
		}
	}
	return true
}
