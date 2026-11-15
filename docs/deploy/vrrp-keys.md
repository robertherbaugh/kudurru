# VRRP HMAC key files

The keepalived supervisor renders file references, not key material, into
`keepalived.conf`. Authentication remains `auth_hmac`, `active_key 1` and
`anti_replay monotonic`. Key selection does not change VRRP timers,
priorities, `nopreempt`, health probes or the VIP provider's ownership rules.

## Helm references

An empty `vip.keepalived.hmacKeySecrets` list preserves the shared-key
behavior: `vip.keepalived.existingSecret` selects a Secret with key
`vrrp-hmac`, or the chart generates and retains its shared Secret. Every
VRID uses the existing `/etc/kudurru/secrets/vrrp/vrrp-hmac` path. Prefer an
existing Secret to putting key material in Helm values.

For independent per-VRID selection, provision keys separately in the
release namespace and reference only their names:

```yaml
edge:
  enabled: true
vip:
  provider: keepalived
  keepalived:
    interface: eth0
    vips:
      - {name: dns-a, address: 10.0.0.53/24, vrid: 53}
      - {name: dns-b, address: 10.0.0.54/24, vrid: 54}
    hmacKeySecrets:
      - {vrid: 53, secretName: vrrp-auth-53, key: hmac}
      - {vrid: 54, secretName: vrrp-auth-54, key: hmac}
```

Create the Secrets from protected files or through your secret manager.
For example, with existing supported HMAC key files:

```sh
kubectl -n kudurru-system create secret generic vrrp-auth-53 \
  --from-file=hmac=/secure/vrrp-53.key
kubectl -n kudurru-system create secret generic vrrp-auth-54 \
  --from-file=hmac=/secure/vrrp-54.key
```

The nonempty list must cover **every** configured VIP VRID exactly once,
including the single-VIP shorthand `vip.address` / `vip.keepalived.vrid`.
There may be at most 255 entries; IDs are integers from 1 through 255.
Duplicate, missing or unknown VRIDs refuse rendering. Each entry accepts
only `vrid`, `secretName` and `key`; raw values and cross-namespace references
are refused. A Secret may hold multiple explicitly selected keys. Selecting
the same Secret/key for multiple VRIDs explicitly shares its material;
there is no implicit shared-key fallback and the chart does not compare or
read key contents.

Clear `existingSecret` and the legacy `hmacSecret` value when selecting this
mode; mixed choices refuse rendering. It requires an active keepalived edge.
No per-VRID Secret is generated or looked up by Helm. Only the selected keys
are projected, with `optional: false`: a missing Secret or key prevents Pod
startup. A successful render proves the mapping, not that those objects
exist or that their key material is valid.

For the example above, the chart passes:

```text
--hmac-key-file=/etc/kudurru/secrets/vrrp/vrrp_hmac_{vrid}
```

The existing CLI expands this independently to `vrrp_hmac_53` and
`vrrp_hmac_54` in their original instances. A missing file is a preflight
failure; it never selects another VRID's file or a shared successor key.

## Private runtime copies

Keepalived 2.4.3 refuses HMAC files readable by group or others. Kubernetes
`fsGroup` ownership handling can add group read permission even when a
Secret requests mode 0400. The edge needs its existing fsGroup for dnsdist,
so the chart stages keys into a separate private memory volume for both
shared and per-VRID modes:

- The `vrrp-keys` init container uses the same keepalived image as the
  supervisor. It runs as root with all capabilities dropped, a read-only
  root filesystem and no privilege escalation. The fixed script has a
  30-second timeout, 100m CPU and 32Mi memory limits.
- It reads only the selected filenames, at most 4097 bytes per file, and
  refuses empty or larger-than-4096-byte inputs. This is a staging bound;
  keepalived still validates the actual cryptographic key format. Up to
  255 files fit within a bounded 2Mi memory volume. The directory is mode
  0700 and each copied file mode 0600. It does not log key bodies or use
  shell tracing.
- Source Secret mounts are visible only to init. The normal keepalived
  container sees only the private copy, mounted read-only. Neither source
  nor private key mounts are exposed to dnsdist. Its existing fsGroup and
  other credential mounts remain unchanged.

An invalid copy stops init before normal containers start. A custom
keepalived image must provide the standard Alpine shell utilities used by
this init (`/usr/bin/timeout`, `/bin/sh`, `head`, `wc`, `chmod`) as well as the
supervisor and keepalived. Keys are not placed in ConfigMaps, command
arguments or environment values.

## Migration and rotation

Provision matching per-VRID key material on **every** participating node;
VRIDs identify the same instances across nodes. Keep existing VRIDs and
all non-key configuration unchanged while migrating. Retain the old shared
Secret until every peer has been verified on the intended selection. The
chart does not delete a retained old shared Secret or use it as fallback.

Private copies are created once per Pod start. Updating a same-name Secret
alone does **not** change the running copy or trigger a restart. Changing a
reference changes the Pod template, but an ordinary rolling update can
leave peers with different keys. The current single active key does not
provide an overlapping key ring or seamless live rotation. Use a coordinated
maintenance window: drain and stop all affected VIP supervisors, update
material/references consistently, recreate the affected Pods, verify their
preflight/readiness and peer ownership, then restore service. Prevent
old-key and new-key peers from advertising the same VIP concurrently. Plan
for DNS/VIP interruption during this procedure; do not weaken HMAC mode to
make mixed keys interoperate.

## Host and Compose equivalent

The existing CLI pattern also works outside Helm:

```text
--hmac-key-file=/run/secrets/vrrp_hmac_{vrid}
```

For the same two VIPs, explicitly mount files at
`/run/secrets/vrrp_hmac_53` and `/run/secrets/vrrp_hmac_54` on every peer,
root-owned and without group/other read permission. Compose can select
these target names from separate file secrets. Keep the full existing
supervisor command, peer discovery, licensing and health configuration:
Compose command lists replace the whole command rather than appending a
flag. The supplied base Compose files retain their existing shared-key
selection. Follow the same coordinated restart rules; neither the CLI nor
Compose substitutes a missing per-VRID file with the shared file.
