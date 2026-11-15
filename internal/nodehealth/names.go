// Package nodehealth defines kudurru's local node-bound DNS health names.
// A target is a bounded DNS label derived from the exact serving owner. It
// identifies an expected node; it does not authenticate an arbitrary peer.
package nodehealth

import (
	"crypto/sha256"
	"encoding/base32"
	"strings"
)

// Generic is the original local health name used by existing nodes and probes.
const Generic = "health.kudurru.internal."

// Namespace reserves local node-targeted health names, including future versions.
const Namespace = "node.health.kudurru.internal."

// Target binds a versioned health question to exact node identity bytes.
// Empty identities cannot provide ownership proof.
func Target(owner string) string {
	if owner == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(owner))
	label := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(digest[:])
	return strings.ToLower(label) + ".v1." + Namespace
}

// Reserved covers the entire local subtree, including malformed/future
// targets, so they cannot escape into recursive resolution or query logs.
func Reserved(name string) bool {
	lower := strings.ToLower(name)
	return lower == Namespace || strings.HasSuffix(lower, "."+Namespace)
}

// IsName covers generic and targeted local health traffic.
func IsName(name string) bool { return strings.EqualFold(name, Generic) || Reserved(name) }

// Matches requires the captured local node owner; the question's spelling
// is case-insensitive while the owner's identity bytes remain case-sensitive.
func Matches(name, owner string) bool {
	target := Target(owner)
	return target != "" && strings.EqualFold(name, target)
}
