# Pinned quic-go source

This directory retains the complete MIT-licensed `github.com/quic-go/quic-go`
v0.63.0 module. `UPSTREAM.json` records the SHA-256 of every original file;
`LICENSE` is the unchanged upstream license. The root Go module uses a local
replacement, and the root Dockerfile includes it before dependency resolution.

The path-retirement change is in `path_manager.go`: retain the active probing
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

The standalone module also pins patched `golang.org/x` dependencies rather
than relying on the application's higher minimum versions. This closes the
upstream module's independently scanned crypto, module transparency-log,
HTTP/2, HTML and text vulnerability findings. The root application's selected
dependency versions are unchanged.

Narrow Semgrep annotations document reviewed upstream runtime contracts:
unsupported-version parsing returns a nonnil header, QUIC enforces TLS 1.3,
and impossible negative frame accounting terminates the connection goroutine
with an unrecovered panic. These annotations change no runtime behavior.
The root scan retains all production QUIC sources; only upstream examples,
integration fixtures, interop tools, dashboard fixtures and inactive upstream
GitHub configuration follow the repository's development-fixture exclusions.
`UPSTREAM.json` remains the original source manifest, including original module
pins, so every local difference remains independently reviewable.

Connection migration also publishes the replacement sender under the same
mutex used by `ConnectionState`; `LocalAddr` and `RemoteAddr` snapshot the sender
under that mutex. Event-loop packet handling retains its existing ownership.
The sender's permanent GSO-refusal flag is atomic because the send queue writes
it while connection/state readers inspect capabilities. Packet fallback and
per-path GSO reset behavior are unchanged.

Hosted race detection exposed unsynchronized replacement during real HTTP/3
migration. New concurrent DoQ/HTTP3 state/address tests reproduced that race on
the original bytes; `send_conn_concurrent_test.go` independently reproduced the
GSO flag race with actual send fallback. Both failed receipts are preserved in
`.context/quic-state-race-20261109/`. Original upstream and application assertions
and `UPSTREAM.json` remain unchanged.
