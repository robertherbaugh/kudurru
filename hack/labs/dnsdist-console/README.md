# dnsdist console-under-YAML acceptance

This disposable Linux runner qualifies the D17/F43c external-program gate. It
starts the pinned dnsdist 2.1.3 as a separate process in Docker with loopback
listeners, two disposable DNS responders and generated one-day certificates.
It does not build Kudurru or certify the production supervisor.

Requirements: Python 3.11+, Docker, OpenSSL and a Linux Docker daemon that can
use host networking. Pull the digest-pinned default image before running:

```sh
docker pull powerdns/dnsdist-21:2.1.3@sha256:c70ae7363712188ab2878d22bc9f1801cae29190688474a224caee2089f74837
python3 hack/labs/dnsdist-console/run.py \
  --result .context/dnsdist-console-result.json
```

`--docker-host` selects an isolated daemon; `--image` selects an already present
image (prefer an immutable digest). The runner checks its dnsdist version. It
uses an unprivileged user, drops capabilities and mounts private generated
configuration read-only. Ports are selected on loopback. Only its label-verified
container is removed. The temporary configuration, private keys and ordinary
child output are deleted on completion. `--keep-private-log` preserves captured
child output in a mode-0600 sibling `.private.log`; do not publish that file.
The JSON report contains fixed result labels and the selected image, without
keys or child output. No homelab or existing service is changed.

The configuration is JSON syntax accepted as YAML, with an empty function-only
Lua sibling. No `enableLuaConfiguration()` call is used. Console calls execute
`dnsdist -C /lab/dnsdist.yml -e <literal command>`; the console key stays in the
private configuration and is never passed with `-k` or in an environment value.

The runner checks:

- Initial UDP and DoT DNS answers from the YAML-configured backend.
- A failed Lua command has no success acknowledgment or DNS effect. Process
  exit status alone is insufficient; a terminal `return` supplies the marker.
  Lua `print` goes to the daemon log rather than the client response.
- `newServer`, `setDown` and `rmServer` change actual UDP and fresh DoT answers.
  Fixture members are forced up; production health checks need separate tests.
- `reloadAllCertificates()` changes the exact certificate on new TLS sessions
  while an existing TLS session still answers DNS.
- Invalid replacement certificates retain the last good TLS context for new
  and established sessions.
- The child PID remains unchanged throughout those mutations.
- Updating persisted YAML converges on the intended member after a deliberate
  restart.

## Executed evidence

On 2026-10-31 all ten checks passed with the repository's locally built
`kudurru-dnsdist` image, immutable ID
`sha256:89c7f196a871779f5df5da3d7057488f3a4f4ce192f21b37a1fa230b7fd5930e`.
It runs dnsdist 2.1.3 from the pinned image above. The final result is recorded
in `dev-docs/milestones/v0.2/gaps.md`; private runtime files are not committed.

The test also exposed a concrete limit: a DoT connection established before a
backend change returned SERVFAIL on its next query after its selected backend
was marked down, while new DoT connections reached the replacement. Live
membership mutation does not guarantee transparent migration of established
connections. Kudurru must not replay those queries. Certificate-only reload
preserved the separately established active connection.

This is capability acceptance, not a benchmark or production failure-ordering
proof. It does not cover PROXYv2, production backend health validators, multiple
TLS listeners, DoH/DoQ/DoH3, cache/rule changes or supervisor recovery. Those
remain independent acceptance requirements for the implementation.
