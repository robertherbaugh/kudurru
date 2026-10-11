// Package internalapi is kudurru's node-to-node listener (":8082",
// docs/architecture.md §3.1 "internal" row, §4.5, decision D47) and its
// client.
//
// The listener is always TLS. Unless overridden by files, the bearer token
// and a CA are derived from the cluster encryption key
// (KUDURRU_ENCRYPTION_KEY) with HKDF-SHA256:
//
//   - token: HKDF(key, info "kudurru/internal/v1"), 32 bytes, base64url;
//   - CA: an Ed25519 key from HKDF(key, info "kudurru/internal-ca/v1") and
//     a deterministic self-signed certificate over it.
//
// Every node holding the key derives the same token and CA, signs its own
// short-lived node certificate (ephemeral Ed25519 key, DNS SAN ServerName)
// with the CA, and trusts every other node, so the chart needs no extra
// Secret. During a key rotation (KUDURRU_ENCRYPTION_KEY_PREVIOUS) both
// tokens are accepted, both CAs trusted, node certificates are issued by
// the previous CA (which every node trusts) and clients retry with the
// previous token on 401, so nodes on either key interoperate while the
// rollout proceeds. listeners.internal.tokenFile and the tls files replace
// the derived material.
//
// Routes:
//
//	GET /internal/status          {node, version, leader, state, lists:[{name, digests}]}
//	GET /internal/lists/{digest}  the cached list body whose sha256 is digest
//
// Requests are rate-limited per client address, carry no body, and list
// downloads are bounded in number and size. The client verifies the sha256
// of every list body while it streams.
package internalapi
