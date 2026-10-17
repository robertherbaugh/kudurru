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
libcap tools (`setcap`, `getcap`), standard shell utilities, and stock dhcpcd
with IPv4 support. Sudo must permit
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
and `NET_RAW`, with `no-new-privileges`. It verifies actual effective, permitted,
bounding and ambient capabilities, then opens an ARP socket without sending a
packet. Both ambient and executable-file capability profiles are checked;
restart runs the server with file capabilities and no ambient capabilities.
These reproduce the privilege profiles used by systemd and the opt-in container
image; they do not activate a systemd manager or Docker daemon.

Cleanup stops recorded lab processes
and removes its namespace handles; the private PID namespace kills remaining
descendants on exit.

Coverage includes two direct subnets, distinct clients, a reservation, relay
acquisition on a shared transit listener, pool exhaustion, release and address
reuse, restart persistence, routed renewal, expiration and reacquisition.
Extra static addresses occupy the first direct and routed pool candidates
before acquisition. The server must detect actual ARP and ICMP responses,
persist declined quarantines, and retain those quarantines after restart.
Packet captures include both probe protocols. The stock client also consumes
NTP, search domains, classless routes and boot settings and supplies a client
FQDN. The client hook records the received options for inspection.

The relay uses a transit `giaddr`, RFC 3527 link selection for the downstream
subnet, RFC 5107 server identifier override to a second server address, and
RFC 8357 source port 1067. Successful acquisition exercises all three extensions
with a stock client. Captures verify replies to the authorized `giaddr:1067`,
the overridden server identifier, and ordinary routed renewals to the server.
Quorum failover is exercised separately by the failover package's integration
tests; DHCPv6 is outside this IPv4 harness.

The `dhcp-acceptance` CI job installs Ubuntu's stock `dhcpcd-base` package,
runs this harness, and uploads its evidence. The printed
`.context/dhcp-acceptance.*` directory retains the client version, a fixed
snapshot of the script, capability checks, logs, lease databases, packet
captures, decoded packets, and a `result` file
containing `PASS` on success. Runtime network resources are removed; evidence
files remain for review. Run the harness entry point rather than invoking the
fixture directly.
