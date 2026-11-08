# Pinned quic-go source

This directory retains the complete MIT-licensed `github.com/quic-go/quic-go`
v0.63.0 module. `UPSTREAM.json` records the SHA-256 of every original file;
`LICENSE` is the unchanged upstream license. The root Go module uses a local
replacement, and the root Dockerfile includes it before dependency resolution.

The local production change is in `path_manager.go`: retain the active probing
path's ID when clearing the candidate path list, then retire that ID on the next
switch. The original implementation lost the reference without releasing its
reserved connection ID. Repeated migrations exhausted the peer's finite supply
and subsequent path validation timed out, including with a single UDP socket.
The ordinary sending CID is independently owned by the connection manager.

`migration_retirement_test.go` exercises twelve successive switches with only
two probing CID slots. Upstream path-manager assertions are unchanged. Kudurru's
DNS tests also exercise actual DoQ/HTTP3 handshakes, Retry, queries and three
successive migrations with single- and multiple-socket controls.

The original failure and candidate evidence are recorded in
`.context/quic-multisocket-20261108/` in the development workspace. The first
diagnostic candidate retired all probing IDs at every switch; that candidate
is not this patch, which retains the existing active-path contract.

When upgrading, compare the replacement against the upstream module and rerun
both the upstream path/CID tests and the real DNS migration tests. Remove the
replacement only when upstream supplies equivalent behavior. Do not edit the
shared Go module cache. No upstream contribution or acceptance is claimed.
