# Prepare a DHCP host

[DHCP guides](dhcp.md) · [Step-by-step setup](../guide/dhcp/README.md)

## Network and privileges

The server receives UDP port 67 on the selected interface and sends replies
to UDP port 68. Unconfigured clients need broadcast replies on that LAN.
A published Docker UDP port or a Kubernetes LoadBalancer does not provide
the required broadcast connectivity.

For a Linux container, use host networking, keep the data directory on a
persistent volume and grant `NET_BIND_SERVICE` and `NET_RAW` to both the
executable and the runtime. `NET_RAW` permits binding to the selected
interface and opening the ARP/ICMP probe sockets. Adding `--cap-add NET_RAW` alone to the existing
non-root DNS image does not give its executable that effective capability.

Published container images do not include the file capabilities required for
DHCPv4. Enabling DHCP in the dashboard does not add them. Build a local
DHCP-capable image from a source checkout:

```sh
make docker-dhcp
```

This builds the local image `kudurru:dhcp-development` with the executable's
file capabilities `cap_net_bind_service,cap_net_raw=ep`. The underlying build
option is `DHCP_CAPABILITIES=true` in
`images/hostnetwork/Dockerfile`; it defaults to `false`.

Prepare the secret files described in the [Docker guide](docker.md#docker-run).
For fresh dashboard setup, run from the directory containing `secrets/`;
no YAML mount is required:

```sh
docker run -d --name kudurru --restart unless-stopped \
  --network host --user 65532:65532 --read-only \
  --security-opt no-new-privileges \
  --cap-drop ALL --cap-add NET_BIND_SERVICE --cap-add NET_RAW \
  --tmpfs /run/kudurru:uid=65532,gid=65532 --tmpfs /tmp \
  -v kudurru-data:/var/lib/kudurru \
  -v "$PWD/secrets:/run/secrets:ro" \
  -e KUDURRU_ENCRYPTION_KEY_FILE=/run/secrets/encryption_key \
  -e KUDURRU_ADMIN_PASSWORD_FILE=/run/secrets/admin_password \
  kudurru:dhcp-development
```

For optional host-file setup, also mount
`$PWD/kudurru.yaml:/etc/kudurru/kudurru.yaml:ro` and append
`serve --config /etc/kudurru/kudurru.yaml`. Mounted configuration and secrets
must be readable by uid 65532. A privileged container is not required. Review
every listener when using host networking because configured addresses now
refer to the host network.

For the packaged systemd service, add a drop-in with `systemctl edit kudurru`:

```ini
[Service]
AmbientCapabilities=CAP_NET_BIND_SERVICE CAP_NET_RAW
CapabilityBoundingSet=CAP_NET_BIND_SERVICE CAP_NET_RAW
# Reset the packaged allowlist, then include ARP packet sockets.
RestrictAddressFamilies=
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX AF_NETLINK AF_PACKET
```

Restart the service after changing its configuration and capabilities. The
default unit and deployment templates do not enable DHCP or grant its extra
capability. A missing interface, a server address absent from that interface,
an occupied UDP port 67 or insufficient permissions prevents DHCP from
starting; dashboard-owned setup retains the [dashboard repair path](dhcp-management.md#repair-a-setup-that-could-not-start).
