# Isolated DHCPv4 acceptance test

This opt-in Linux harness uses a separately installed, stock **dhcpcd** client
(BSD-2-Clause). It does not vendor client source or binaries and is not run by
`go test`.

From the repository root, run:

```sh
DHCPCD=/absolute/path/to/dhcpcd bash test/dhcp/run.sh
```

If dhcpcd is already on `PATH`, `bash test/dhcp/run.sh` finds it automatically.
Allow about two minutes for real lease expiration. Missing dependencies or
privileges fail the run; they do not silently skip checks.

Requirements: Linux network/mount/PID namespaces, Bash, Go, sudo, iproute2
(`ip`), `mount`, `sysctl`, `jq`, `tcpdump`, util-linux (`unshare`, `setpriv`),
standard shell utilities, and stock dhcpcd with IPv4 support. Sudo must permit
namespace, mount, interface, and route setup. No Docker daemon is needed.

The harness first creates a disconnected outer network namespace and refuses
to continue unless it differs from the host namespace, contains only loopback,
and has no routes. Every test interface, bridge, route, and forwarding change
stays inside that boundary. It never joins a host interface, creates a host
bridge, or sends DHCP on the live LAN. The fixture independently checks the
namespace marker before opening sockets.

Private mount namespaces isolate namespace handles and each client's `/run`
and `/var` state. An explicit event-only dhcpcd hook avoids changing host DNS or
hostname settings. The server runs as UID 65532 with only `NET_BIND_SERVICE`
and `NET_RAW`, with `no-new-privileges`. Cleanup stops recorded lab processes
and removes its namespace handles; the private PID namespace kills remaining
descendants on exit.

Coverage includes two direct subnets, distinct clients, a reservation, relay
acquisition on a shared transit listener, pool exhaustion, release and address
reuse, restart persistence, routed renewal, expiration and reacquisition.
Packet captures verify replies to the relay's `giaddr:67` and ordinary routed
renewals to the server. The relay fixture implements ordinary trusted giaddr
forwarding; link selection, server-ID override, alternate relay ports, DHCPv6,
and failover are outside this test and server slice.

The printed `.context/dhcp-acceptance.*` directory retains client version,
logs, lease databases, packet captures, decoded packets, and a `result` file
containing `PASS` on success. Runtime network resources are removed; evidence
files remain for review. Run the harness entry point rather than invoking the
fixture directly.
