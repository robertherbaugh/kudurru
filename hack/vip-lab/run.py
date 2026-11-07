#!/usr/bin/env python3
"""Disposable two-namespace VIP acceptance; never substitutes for two VMs."""

import argparse
from datetime import datetime, timezone
import ipaddress
import json
from pathlib import Path
import re
import secrets
import shutil
import signal
import socket
import struct
import subprocess
import tempfile
import threading
import time
import urllib.error
import urllib.request
import uuid

LABEL = "io.kudurru.vip-lab.run"
ROOT = Path(__file__).resolve().parents[2]
QNAME = "vip-check.lab.invalid."
ANSWER = "203.0.113.68"
SAMPLE = 0.1
# Unchanged generated timers: advert 1s, fall 3, script timeout 1s.
# Both configured backup priorities are 190. Include 1s observation allowance
# for Docker interface inspection and the 250ms DNS client timeout.
DOWN = 3 + (256 - 190) / 256
BOUNDS = {
    "hard_node_loss": DOWN + 1,
    "drain": 1 + DOWN + 1,
    "edge_failure": 3 + 1 + DOWN + 1,
    "graceful_stop": 15,
}
LIMITATIONS = [
    "Containers share one Linux kernel; original two-VM gate remains open.",
    "Identical independent SQLite seeds; no replicated SQLite HA or update-loss guarantee.",
    "IPv4 UDP/TCP only; no IPv6, encrypted client transports, Windows or client-policy lab.",
    "No HMAC wire spoof/replay/rotation attack qualification.",
    "No stale-snapshot, hypervisor/power-loss, NIC/switch or real-host firewall qualification.",
    "Ownership observations are sampled, not a proof against all transient split-brain.",
]


class Failure(Exception):
    """Only runner-authored, credential-free error labels reach the result."""


def command(argv, timeout=20, docker_host=None, check=True):
    env = (
        None
        if docker_host is None
        else {"DOCKER_HOST": docker_host, "PATH": "/usr/local/bin:/usr/bin:/bin"}
    )
    try:
        proc = subprocess.run(
            argv,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            timeout=timeout,
            check=False,
            env=env,
        )
    except subprocess.TimeoutExpired:
        raise Failure("command_timeout") from None
    if check and proc.returncode:
        # Never echo argv or captured child output, even in exceptions.
        categories = {
            b"bind source path does not exist": "missing_bind_source",
            b"permission denied": "permission_denied",
            b"executable file not found": "missing_executable",
            b"cannot allocate memory": "memory",
            b"no such file or directory": "missing_path",
            b"invalid mount config": "invalid_mount",
            b"operation not permitted": "capability",
            b"not a directory": "mount_type",
            b"already in use": "resource_in_use",
        }
        category = next(
            (
                label
                for text, label in categories.items()
                if text in proc.stderr.lower()
            ),
            "unclassified",
        )
        raise Failure("command_exit_" + str(proc.returncode) + "_" + category)
    return proc


def read_name(data, offset):
    """Decode a bounded RFC1035 name and reject pointer loops/truncation."""
    labels, visited, end = [], set(), None
    for _ in range(128):
        if offset >= len(data) or offset in visited:
            raise Failure("malformed_dns")
        visited.add(offset)
        size = data[offset]
        offset += 1
        if size & 0xC0 == 0xC0:
            if offset >= len(data):
                raise Failure("malformed_dns")
            end = end if end is not None else offset + 1
            offset = ((size & 63) << 8) | data[offset]
            continue
        if size & 0xC0 or size > 63 or offset + size > len(data):
            raise Failure("malformed_dns")
        if not size:
            return b".".join(labels).lower() + b".", end if end is not None else offset
        labels.append(data[offset : offset + size])
        if sum(map(len, labels)) + len(labels) > 254:
            raise Failure("malformed_dns")
        offset += size
    raise Failure("malformed_dns")


def validate_answer(data, ident):
    if len(data) < 12:
        raise Failure("malformed_dns")
    rid, flags, qd, an, _, _ = struct.unpack_from("!6H", data)
    if rid != ident or not flags & 0x8000 or flags & 0x0200 or flags & 15 or qd != 1:
        raise Failure("dns_unavailable")
    question, offset = read_name(data, 12)
    if offset + 4 > len(data):
        raise Failure("malformed_dns")
    if question != QNAME.encode() or struct.unpack_from("!HH", data, offset) != (1, 1):
        raise Failure("wrong_dns_answer")
    offset += 4
    addresses = []
    for _ in range(an):
        owner, offset = read_name(data, offset)
        if offset + 10 > len(data):
            raise Failure("malformed_dns")
        kind, cls, _, size = struct.unpack_from("!HHIH", data, offset)
        offset += 10
        if offset + size > len(data):
            raise Failure("malformed_dns")
        if owner != QNAME.encode():
            raise Failure("wrong_dns_answer")
        if kind == 1 and cls == 1 and size == 4:
            addresses.append(socket.inet_ntoa(data[offset : offset + 4]))
        offset += size
    if addresses != [ANSWER] or an != 1 or not flags & 0x0400:
        raise Failure("wrong_dns_answer")


def dns_query(address, tcp=False):
    ident = secrets.randbits(16)
    name = (
        b"".join(
            bytes([len(s)]) + s.encode("ascii") for s in QNAME.rstrip(".").split(".")
        )
        + b"\0"
    )
    query = struct.pack("!6H", ident, 0, 1, 0, 0, 0) + name + struct.pack("!2H", 1, 1)
    with socket.socket(
        socket.AF_INET, socket.SOCK_STREAM if tcp else socket.SOCK_DGRAM
    ) as conn:
        conn.settimeout(0.25)
        conn.connect((address, 53))
        if tcp:
            conn.sendall(struct.pack("!H", len(query)) + query)
            size = struct.unpack("!H", recv_exact(conn, 2))[0]
            data = recv_exact(conn, size)
        else:
            conn.send(query)
            data = conn.recv(65535)
    validate_answer(data, ident)


def recv_exact(conn, count):
    data = bytearray()
    while len(data) < count:
        part = conn.recv(count - len(data))
        if not part:
            raise Failure("short_dns_tcp")
        data.extend(part)
    return data


class DNSMonitor:
    def __init__(self, vips):
        self.vips = vips
        self.stop = threading.Event()
        self.lock = threading.Lock()
        self.samples = []
        self.threads = []
        self.saturated = False

    def start(self):
        for name, address in self.vips.items():
            for protocol in ("udp", "tcp"):
                thread = threading.Thread(
                    target=self.watch, args=(name, address, protocol), daemon=True
                )
                self.threads.append(thread)
                thread.start()

    def watch(self, name, address, protocol):
        while not self.stop.is_set():
            outcome = "ok"
            try:
                dns_query(address, protocol == "tcp")
            except (Failure, OSError) as exc:
                outcome = str(exc) if isinstance(exc, Failure) else "transport"
            with self.lock:
                if len(self.samples) >= 20000:
                    self.saturated = True
                    self.stop.set()
                    return
                self.samples.append((time.monotonic(), name, protocol, outcome))
            self.stop.wait(SAMPLE)

    def summary(self, start, end):
        with self.lock:
            samples = list(self.samples)
        streams = {}
        for name in self.vips:
            for protocol in ("udp", "tcp"):
                stream = [s for s in samples if s[1:3] == (name, protocol)]
                during = [s for s in stream if start <= s[0] <= end]
                preceding = [s[0] for s in stream if s[0] < start and s[3] == "ok"]
                last = preceding[-1] if preceding else start
                gaps = []
                for stamp, _, _, outcome in during:
                    if outcome == "ok":
                        gaps.append(stamp - last)
                        last = stamp
                gaps.append(end - last)
                streams[name + "/" + protocol] = {
                    "samples": len(during),
                    "successes": sum(s[3] == "ok" for s in during),
                    "failures": sum(s[3] != "ok" for s in during),
                    "wrong_answers": sum(
                        s[3] in ("wrong_dns_answer", "malformed_dns") for s in during
                    ),
                    "max_success_gap_s": round(max(gaps), 4),
                }
        return streams

    def close(self):
        self.stop.set()
        for thread in self.threads:
            thread.join(1)


class Lab:
    def __init__(self, args):
        self.args = args
        self.run_id = uuid.uuid4().hex[:12]
        self.prefix = "kudurru-vip-lab-" + self.run_id
        self.resources = []
        self.active = {"a": True, "b": True}
        self.addresses = {}
        self.vips = {}
        self.private = Path(tempfile.mkdtemp(prefix=self.prefix + "-"))
        self.http = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        self.started = time.monotonic()
        self.result = {
            "schema": 1,
            "started_at": datetime.now(timezone.utc).isoformat(),
            "run_id": self.run_id,
            "application_sha": args.app_sha,
            "container_evidence_only": True,
            "bounds_s": BOUNDS,
            "dns_sample_interval_s": SAMPLE,
            "dns_timeout_s": 0.25,
            "limitations": LIMITATIONS,
            "cases": [],
        }
        self.monitor = None
        self.result["stage"] = "setup"

    def docker(self, *args, timeout=20, check=True):
        return command(["docker", *args], timeout, self.args.docker_host, check)

    def create(self, kind, name, extra):
        # Reserve ownership before creation, so failures after daemon acceptance
        # are still cleaned up. UUID plus label prevents unrelated deletion.
        self.resources.append((kind, name))
        return self.docker(
            kind, "create", "--label", LABEL + "=" + self.run_id, *extra, name
        )

    def start_container(self, name, image, argv, extra):
        self.result["stage"] = "create_" + name.removeprefix(self.prefix + "-")
        self.resources.append(("container", name))
        self.docker(
            "run",
            "-d",
            "--pull=never",
            "--name",
            name,
            "--label",
            LABEL + "=" + self.run_id,
            "--env",
            "GOMAXPROCS=2",
            "--log-driver=local",
            "--log-opt",
            "max-size=2m",
            "--log-opt",
            "max-file=1",
            "--log-opt",
            "compress=false",
            *extra,
            image,
            *argv,
        )

    def name(self, node, role):
        return self.prefix + "-" + node + "-" + role

    def request(self, node, port, path, json_body=True):
        try:
            with self.http.open(
                f"http://{self.addresses[node]}:{port}{path}", timeout=0.5
            ) as response:
                data = response.read(128 * 1024 + 1)
                if len(data) > 128 * 1024:
                    raise Failure("oversize_operational_response")
                return json.loads(data) if json_body else data.decode("utf-8")
        except (OSError, urllib.error.URLError, ValueError):
            return None

    def wait(self, predicate, budget, reason):
        end = time.monotonic() + budget
        while time.monotonic() < end:
            if predicate():
                return
            time.sleep(0.15)
        raise Failure(reason)

    def setup(self):
        images = {
            "backend": self.args.backend_image,
            "edge": self.args.edge_image,
            "vip": self.args.vip_image,
        }
        self.result["images"] = {}
        for role, image in images.items():
            ident = (
                self.docker("image", "inspect", "--format", "{{.Id}}", image)
                .stdout.decode()
                .strip()
            )
            if not re.fullmatch(r"sha256:[0-9a-f]{64}", ident):
                raise Failure("invalid_image_identity")
            self.result["images"][role] = {"reference": image, "id": ident}
        # Short-lived image tools use no runtime host network and no ports.
        for role, image, binary, argv in (
            (
                "application",
                images["backend"],
                "/ko-app/kudurru",
                ["version", "--json"],
            ),
            (
                "edge_application",
                images["edge"],
                "/usr/local/bin/kudurru",
                ["version", "--json"],
            ),
            (
                "vip_application",
                images["vip"],
                "/usr/local/bin/kudurru",
                ["version", "--json"],
            ),
            ("dnsdist", images["edge"], "/usr/local/bin/dnsdist", ["--version"]),
            ("keepalived", images["vip"], "/usr/sbin/keepalived", ["--version"]),
        ):
            name = self.prefix + "-version-" + role
            self.resources.append(("container", name))
            p = self.docker(
                "run",
                "--rm",
                "--pull=never",
                "--name",
                name,
                "--network=none",
                "--label",
                LABEL + "=" + self.run_id,
                "--entrypoint",
                binary,
                image,
                *argv,
            )
            if role in ("application", "edge_application", "vip_application"):
                info = json.loads(p.stdout)
                if info["commit"] != self.args.app_sha:
                    raise Failure("application_sha_mismatch")
                self.result["build" if role == "application" else role + "_build"] = {
                    k: info[k] for k in ("version", "commit", "goVersion", "platform")
                }
            else:
                match = re.search(
                    rb"(?:dnsdist |Keepalived v)([0-9]+\.[0-9]+\.[0-9]+)",
                    p.stdout + p.stderr,
                )
                if (
                    not match
                    or match.group(1).decode()
                    != {"dnsdist": "2.1.3", "keepalived": "2.4.3"}[role]
                ):
                    raise Failure("external_version_mismatch_" + role)
                self.result[role + "_version"] = match.group(1).decode()
        self.result["stage"] = "network"
        network = self.prefix + "-net"
        self.create("network", network, ["--internal", "--driver", "bridge"])
        ipam = json.loads(
            self.docker(
                "network", "inspect", "--format", "{{json .IPAM.Config}}", network
            ).stdout
        )
        subnet = next(
            ipaddress.ip_network(p["Subnet"]) for p in ipam if ":" not in p["Subnet"]
        )
        if not subnet.is_private or subnet.num_addresses < 128:
            raise Failure("unsuitable_automatic_subnet")
        self.vips = {
            "dns": str(subnet.network_address + 53),
            "dns2": str(subnet.network_address + 54),
        }
        secrets_dir = self.private / "secrets"
        secrets_dir.mkdir(mode=0o755)
        proxy = secrets_dir / "proxy"
        proxy.write_text(secrets.token_hex(32))
        command(["sudo", "chown", "65532:953", str(proxy)])
        command(["sudo", "chmod", "0440", str(proxy)])
        # File-based disposable credentials avoid printing a generated admin password.
        for filename in ("admin", "encryption"):
            path = secrets_dir / filename
            path.write_text(secrets.token_hex(32))
            command(["sudo", "chown", "65532:65532", str(path)])
            command(["sudo", "chmod", "0400", str(path)])
        for vrid in (53, 54):
            path = secrets_dir / ("vrrp_hmac_" + str(vrid))
            path.write_text(secrets.token_hex(16))
            command(["sudo", "chown", "0:0", str(path)])
            command(["sudo", "chmod", "0600", str(path)])
        for node in ("a", "b"):
            for suffix in ("run", "data"):
                self.create("volume", self.name(node, suffix), [])
            self.start_namespace(node)
            self.addresses[node] = self.node_address(node)
        if len(set(self.addresses.values()) | set(self.vips.values())) != 4:
            raise Failure("lab_address_collision")
        self.result["network"] = {
            "subnet": str(subnet),
            "nodes": self.addresses,
            "vips": self.vips,
        }
        for node in ("a", "b"):
            config = {
                "node": {"name": "lab-" + node, "dataDir": "/var/lib/kudurru"},
                "store": {"driver": "sqlite"},
                "objectsDir": "",
                "listeners": {
                    "dns": [{"address": "0.0.0.0:5353", "protocols": ["udp", "tcp"]}],
                    "api": {"address": "0.0.0.0:8080", "tls": {"auto": "off"}},
                    "ops": {"address": "0.0.0.0:8081"},
                    "proxyProtocolFrom": [ip + "/32" for ip in self.addresses.values()],
                    "proxyProtocolSecretFile": "/run/secrets/proxy",
                },
                "auth": {
                    "providerResources": {"systemTrust": False, "allowedOrigins": []}
                },
                "blocking": {"enabled": False},
                "log": {"level": "warn", "format": "json"},
                "objects": [
                    {
                        "apiVersion": "kudurru.io/v1alpha1",
                        "kind": "DNSZone",
                        "metadata": {"name": "lab"},
                        "spec": {
                            "zoneName": "lab.invalid.",
                            "type": "primary",
                            "nameservers": ["ns.lab.invalid."],
                        },
                    },
                    {
                        "apiVersion": "kudurru.io/v1alpha1",
                        "kind": "DNSRecordSet",
                        "metadata": {"name": "answer", "namespace": "default"},
                        "spec": {
                            "zoneRef": {"name": "lab"},
                            "endpoints": [
                                {
                                    "dnsName": QNAME,
                                    "recordType": "A",
                                    "targets": [ANSWER],
                                }
                            ],
                        },
                    },
                ],
            }
            path = self.private / (node + ".json")
            path.write_text(json.dumps(config))
            path.chmod(0o444)
            self.start_backend(node)
            self.start_edge(node)
        self.wait(
            lambda: all(self.edge_up(n, 2) for n in ("a", "b")),
            60,
            "edge_startup_unready",
        )
        self.result["stage"] = "vip_startup"
        for node in ("a", "b"):
            self.start_vip(node)
        self.wait(lambda: self.steady_snapshot() is not None, 20, "initial_vip_unready")
        for node in ("a", "b"):
            doctor = self.cli(node, "doctor", "--skip-api", "--skip-ports")
            if (
                "vrrp: OK 2 VRRP instances authenticated with auth_hmac"
                not in doctor.stdout.decode()
            ):
                raise Failure("doctor_did_not_confirm_auth_hmac")
        self.result["doctor_vrrp_nonlegacy"] = True
        self.result["generated_supervisor_preflights"] = True
        self.monitor = DNSMonitor(self.vips)
        self.monitor_started = time.monotonic()
        self.monitor.start()

    def start_namespace(self, node):
        self.start_container(
            self.name(node, "ns"),
            self.args.vip_image,
            ["3600"],
            [
                "--network",
                self.prefix + "-net",
                "--entrypoint",
                "/bin/sleep",
                "--mount",
                f"type=volume,src={self.name(node, 'run')},dst=/run/kudurru",
                "--cap-add=NET_ADMIN",
                "--cap-add=NET_RAW",
                "--cap-add=NET_BROADCAST",
            ],
        )

    def start_backend(self, node):
        self.start_container(
            self.name(node, "backend"),
            self.args.backend_image,
            ["serve", "--config", "/etc/kudurru/lab.json"],
            [
                "--network",
                "container:" + self.name(node, "ns"),
                "--env",
                "KUDURRU_LICENSE_KEY_FILE=/run/test-license",
                "--env",
                "KUDURRU_ADMIN_PASSWORD_FILE=/run/credentials/admin",
                "--env",
                "KUDURRU_ENCRYPTION_KEY_FILE=/run/credentials/encryption",
                "--mount",
                f"type=bind,src={self.private / 'secrets' / 'admin'},dst=/run/credentials/admin,readonly",
                "--mount",
                f"type=bind,src={self.private / 'secrets' / 'encryption'},dst=/run/credentials/encryption,readonly",
                "--mount",
                f"type=bind,src={self.args.license_file},dst=/run/test-license,readonly",
                "--mount",
                f"type=bind,src={self.private / 'secrets' / 'proxy'},dst=/run/secrets/proxy,readonly",
                "--mount",
                f"type=bind,src={self.private / (node + '.json')},dst=/etc/kudurru/lab.json,readonly",
                "--mount",
                f"type=volume,src={self.name(node, 'data')},dst=/var/lib/kudurru",
            ],
        )

    def node_address(self, node):
        field = (
            '{{(index .NetworkSettings.Networks "' + self.prefix + '-net").IPAddress}}'
        )
        address = (
            self.docker(
                "container", "inspect", "--format", field, self.name(node, "ns")
            )
            .stdout.decode()
            .strip()
        )
        if not ipaddress.ip_address(address).is_private:
            raise Failure("nonprivate_node_address")
        return address

    def common(self, node):
        return [
            "--network",
            "container:" + self.name(node, "ns"),
            "--env",
            "KUDURRU_LICENSE_KEY_FILE=/run/test-license",
            "--mount",
            f"type=bind,src={self.args.license_file},dst=/run/test-license,readonly",
            "--mount",
            f"type=volume,src={self.name(node, 'run')},dst=/run/kudurru",
        ]

    def start_edge(self, node):
        self.start_container(
            self.name(node, "edge"),
            self.args.edge_image,
            [
                "/usr/local/bin/kudurru",
                "edge",
                "run",
                "--backends",
                ",".join(
                    "lab-" + n + "=" + ip + ":5353" for n, ip in self.addresses.items()
                ),
                "--poll-zones=false",
                "--authoritative-zones=lab.invalid.",
                "--api-key-out=/run/kudurru/dnsdist-api.key",
                "--proxy-protocol-secret-file=/run/secrets/proxy",
                "--log-level=warn",
            ],
            [
                *self.common(node),
                "--mount",
                f"type=bind,src={self.private / 'secrets' / 'proxy'},dst=/run/secrets/proxy,readonly",
            ],
        )

    def start_vip(self, node):
        self.start_container(
            self.name(node, "vip"),
            self.args.vip_image,
            [
                "/usr/local/bin/kudurru",
                "vip",
                "run",
                "--router-id=lab-" + node,
                "--priority-index=" + str(node == "b" and 1 or 0),
                "--node-count=2",
                "--unicast-src=" + self.addresses[node],
                "--peers=" + self.addresses["b" if node == "a" else "a"],
                "--vip=dns=" + self.vips["dns"] + "/32@eth0#53",
                "--vip=dns2=" + self.vips["dns2"] + "/32@eth0#54",
                "--log-level=warn",
            ],
            [
                *self.common(node),
                "--cap-add=NET_ADMIN",
                "--cap-add=NET_RAW",
                "--cap-add=NET_BROADCAST",
                "--mount",
                f"type=bind,src={self.private / 'secrets' / 'vrrp_hmac_53'},dst=/run/secrets/vrrp_hmac_53,readonly",
                "--mount",
                f"type=bind,src={self.private / 'secrets' / 'vrrp_hmac_54'},dst=/run/secrets/vrrp_hmac_54,readonly",
            ],
        )

    def edge_up(self, node, expected):
        status = self.request(node, 8084, "/status")
        if not status or not status.get("ready"):
            return False
        servers = status.get("dnsdist", {}).get("servers", [])
        # A backend can serve multiple pools; count unique addresses.
        addresses = {
            b.get("address") for b in servers if b.get("state", "").lower() == "up"
        }
        configured = {b.get("address") for b in servers}
        return (
            configured == {ip + ":5353" for ip in self.addresses.values()}
            and len(addresses) == expected
        )

    def owners(self):
        owners = {vip: [] for vip in self.vips}
        for node in ("a", "b"):
            if not self.active[node]:
                continue
            data = json.loads(
                self.docker(
                    "exec",
                    self.name(node, "ns"),
                    "ip",
                    "-j",
                    "address",
                    "show",
                    "dev",
                    "eth0",
                ).stdout
            )
            addresses = {
                a["local"] for interface in data for a in interface.get("addr_info", [])
            }
            for vip, address in self.vips.items():
                if address in addresses:
                    owners[vip].append(node)
        return owners

    def steady_snapshot(self, wanted=None, recovered=False):
        owners = self.owners()
        if any(len(nodes) != 1 for nodes in owners.values()):
            return None
        if wanted and any(owners[vip] != [node] for vip, node in wanted.items()):
            return None
        for node in ("a", "b"):
            if not self.active[node]:
                continue
            status = self.request(node, 8085, "/status")
            metrics = self.request(node, 8085, "/metrics", False)
            if not status or not status.get("ready") or metrics is None:
                return None
            for vip in self.vips:
                master = owners[vip] == [node]
                state = (
                    status.get("state", {}).get("vips", {}).get(vip, {}).get("state")
                )
                if recovered and state not in ("MASTER", "BACKUP"):
                    return None
                if (state == "MASTER") != master or state not in (
                    "MASTER",
                    "BACKUP",
                    "FAULT",
                ):
                    return None
                if f'kudurru_vip_master{{vip="{vip}"}} {int(master)}' not in metrics:
                    return None
        return {vip: nodes[0] for vip, nodes in owners.items()}

    def cli(self, node, *args, role="vip", timeout=20):
        return self.docker(
            "exec",
            self.name(node, role),
            "/usr/local/bin/kudurru" if role != "backend" else "/ko-app/kudurru",
            *args,
            timeout=timeout,
        )

    def settle(self, wanted=None, duration=2, recovered=False):
        end = time.monotonic() + duration
        while time.monotonic() < end:
            if self.steady_snapshot(wanted, recovered=recovered) is None:
                raise Failure("steady_ownership_or_fifo_metric_mismatch")
            for vip in self.vips.values():
                for tcp in (False, True):
                    try:
                        dns_query(vip, tcp)
                    except OSError:
                        raise Failure("steady_dns_transport_failure") from None
            time.sleep(0.1)

    def case(self, name, action, wanted, bound, settle=2, recovered=False):
        start = time.monotonic()
        entry = {"name": name, "passed": False, "bound_s": round(bound, 4)}
        self.result["stage"] = name
        self.result["cases"].append(entry)
        action()
        self.wait(
            lambda: self.steady_snapshot(wanted, recovered=recovered) is not None,
            max(0.01, bound - (time.monotonic() - start)),
            "transition_bound_exceeded",
        )
        entry["ownership_settled_s"] = round(time.monotonic() - start, 4)
        if entry["ownership_settled_s"] > bound:
            raise Failure("transition_bound_exceeded")
        self.settle(wanted, settle, recovered=recovered)
        end = time.monotonic()
        entry["owners"] = self.steady_snapshot(wanted)
        entry["dns"] = self.monitor.summary(start, end)
        if any(
            s["wrong_answers"] or s["max_success_gap_s"] > bound or not s["successes"]
            for s in entry["dns"].values()
        ):
            raise Failure("dns_interruption_bound_or_answer_failed")
        entry["passed"] = True

    def exercise(self):
        self.case("initial_two_vips", lambda: None, None, 2)
        # nopreempt does not promise automatic preferred split after boot.
        split = {"dns": "a", "dns2": "b"}

        def rebalance():
            for node in ("a", "b"):
                self.cli(node, "vip", "rebalance")

        self.case("initial_explicit_rebalance", rebalance, split, BOUNDS["drain"])
        self.wait(
            lambda: self.steady_snapshot(split, recovered=True) is not None,
            8,
            "split_probe_recovery_failed",
        )
        self.settle(split)

        def lameduck():
            self.cli("a", "lameduck", role="backend")

            def retained_and_removed():
                if self.steady_snapshot(split) is None:
                    raise Failure("lameduck_changed_vip_ownership")
                return all(self.edge_up(n, 1) for n in ("a", "b"))

            self.wait(retained_and_removed, 5, "lameduck_backend_not_removed")

        self.case("backend_lameduck_retains_vips", lameduck, split, 6, settle=4)
        self.cli("a", "lameduck", "--cancel", role="backend")
        self.wait(
            lambda: all(self.edge_up(n, 2) for n in ("a", "b")),
            8,
            "backend_recovery_failed",
        )
        # Leave namespace/VIP supervisor alive: only the actual edge disappears.
        self.case(
            "local_edge_failure_probe_demotes",
            lambda: self.docker("stop", "--time", "10", self.name("a", "edge")),
            {"dns": "b", "dns2": "b"},
            BOUNDS["edge_failure"],
        )
        failed = self.request("a", 8085, "/status")
        if (
            not failed
            or any(
                v.get("state") != "FAULT"
                for v in failed.get("state", {}).get("vips", {}).values()
            )
            or len(failed.get("state", {}).get("vips", {})) != 2
        ):
            raise Failure("edge_loss_did_not_fault_both_local_probes")
        self.result["edge_failure_probe_fault"] = True
        self.docker("start", self.name("a", "edge"))
        self.wait(lambda: self.edge_up("a", 2), 30, "edge_recovery_failed")
        self.wait(
            lambda: self.steady_snapshot(recovered=True) is not None,
            8,
            "probe_recovery_failed",
        )
        self.case(
            "edge_recovery_nopreempt",
            lambda: None,
            {"dns": "b", "dns2": "b"},
            2,
            settle=5,
            recovered=True,
        )
        self.case(
            "explicit_rebalance",
            lambda: self.cli("b", "vip", "rebalance"),
            split,
            BOUNDS["drain"],
        )

        # Observe node a's address removal directly after graceful supervisor stop.
        def graceful():
            self.docker("stop", "--time", "20", self.name("a", "vip"), timeout=22)
            self.active["a"] = (
                False  # skip stopped supervisor's status, not its interface
            )
            if any(
                "a" in owners for owners in self.owners_including_stopped().values()
            ):
                raise Failure("graceful_stop_left_kernel_vip")

        self.case(
            "graceful_vip_shutdown",
            graceful,
            {"dns": "b", "dns2": "b"},
            BOUNDS["graceful_stop"],
        )
        self.docker("start", self.name("a", "vip"))
        self.active["a"] = True
        self.wait(
            lambda: self.steady_snapshot(recovered=True) is not None,
            12,
            "vip_recovery_failed",
        )
        self.case(
            "vip_recovery_nopreempt",
            lambda: None,
            {"dns": "b", "dns2": "b"},
            2,
            settle=5,
            recovered=True,
        )
        self.case(
            "explicit_drain",
            lambda: self.cli("b", "vip", "drain", "dns"),
            split,
            BOUNDS["drain"],
        )
        self.cli("b", "vip", "drain", "dns", "--undo")
        self.wait(
            lambda: self.steady_snapshot(split) is not None, 8, "drain_undo_failed"
        )

        def hard_loss():
            # Kill/remove every process sharing the node namespace. Merely
            # killing keepalived would leave its kernel VIP assigned.
            names = [self.name("a", role) for role in ("vip", "edge", "backend", "ns")]
            self.docker("kill", "--signal=KILL", *names)
            self.docker("container", "rm", "--force", "--volumes", *names)
            self.active["a"] = False
            self.result["hard_loss_all_node_containers_removed"] = True

        self.case(
            "whole_node_hard_loss",
            hard_loss,
            {"dns": "b", "dns2": "b"},
            BOUNDS["hard_node_loss"],
        )
        # A fresh namespace restores eth0. Docker reconnect after disconnect
        # instead names the interface eth1, correctly faulting an eth0 VIP.
        self.start_namespace("a")
        if self.node_address("a") != self.addresses["a"]:
            raise Failure("recovered_node_address_changed")
        self.start_backend("a")
        self.start_edge("a")
        self.wait(lambda: self.edge_up("a", 2), 30, "whole_node_edge_recovery_failed")
        self.start_vip("a")
        self.active["a"] = True
        self.wait(
            lambda: self.edge_up("a", 2)
            and self.steady_snapshot(recovered=True) is not None,
            30,
            "whole_node_recovery_failed",
        )
        self.case(
            "whole_node_recovery_nopreempt",
            lambda: None,
            {"dns": "b", "dns2": "b"},
            2,
            settle=5,
            recovered=True,
        )
        self.case(
            "final_explicit_rebalance",
            lambda: self.cli("b", "vip", "rebalance"),
            split,
            BOUNDS["drain"],
        )

    def owners_including_stopped(self):
        active = self.active.copy()
        try:
            self.active = {"a": True, "b": True}
            return self.owners()
        finally:
            self.active = active

    def diagnostics(self):
        """Allowlisted liveness only; never collect container arguments/logs."""
        states = {}
        for kind, name in self.resources:
            if kind != "container":
                continue
            proc = self.docker(
                "container",
                "inspect",
                "--format",
                "{{.State.Running}} {{.State.ExitCode}}",
                name,
                check=False,
            )
            fields = proc.stdout.decode().strip().split()
            if (
                proc.returncode == 0
                and len(fields) == 2
                and fields[0] in ("true", "false")
                and fields[1].isdigit()
            ):
                states[name.removeprefix(self.prefix + "-")] = {
                    "running": fields[0] == "true",
                    "exit_code": int(fields[1]),
                }
        self.result["container_states"] = states
        if self.vips:
            self.result["observed_owners"] = self.owners()
        vip_states, edge_states = {}, {}
        for node in self.addresses:
            if not self.active[node]:
                continue
            status = self.request(node, 8085, "/status")
            if status:
                vip_states[node] = {
                    vip: {
                        key: value
                        for key, value in data.items()
                        if key in ("state", "priority")
                    }
                    for vip, data in status.get("state", {}).get("vips", {}).items()
                }
            status = self.request(node, 8084, "/status")
            if status:
                edge_states[node] = {
                    "ready": status.get("ready"),
                    "backend_states": [
                        b.get("state")
                        for b in status.get("dnsdist", {}).get("servers", [])
                    ],
                }
        self.result["vip_states"] = vip_states
        self.result["edge_states"] = edge_states

    def cleanup(self):
        if self.monitor:
            self.monitor.close()
            if self.monitor.saturated:
                self.result.update(passed=False, monitor_saturated=True)
            self.result["overall_dns"] = self.monitor.summary(
                self.monitor_started, time.monotonic()
            )
            if any(
                s["wrong_answers"]
                or not s["successes"]
                or s["max_success_gap_s"] > max(BOUNDS.values())
                for s in self.result["overall_dns"].values()
            ):
                self.result.update(passed=False, continuity_failed=True)
        failures = []
        for kind, name in reversed(self.resources):
            field = ".Config.Labels" if kind == "container" else ".Labels"
            try:
                p = self.docker(
                    kind,
                    "inspect",
                    "--format",
                    "{{index " + field + ' "' + LABEL + '"}}',
                    name,
                    check=False,
                )
                if p.returncode:
                    continue
                if p.stdout.decode().strip() != self.run_id or not name.startswith(
                    self.prefix + "-"
                ):
                    failures.append("ownership_mismatch")
                    continue
                args = (
                    [kind, "rm"]
                    + (["--force", "--volumes"] if kind == "container" else [])
                    + [name]
                )
                p = self.docker(*args, check=False)
                if p.returncode:
                    failures.append(kind + "_cleanup_failed")
            except Failure:
                failures.append(kind + "_cleanup_command_failed")
        remaining = {}
        for kind, field in (
            ("container", "Names"),
            ("network", "Name"),
            ("volume", "Name"),
        ):
            try:
                proc = self.docker(
                    kind,
                    "ls",
                    *(["-a"] if kind == "container" else []),
                    "--filter",
                    "label=" + LABEL + "=" + self.run_id,
                    "--format",
                    "{{." + field + "}}",
                    check=False,
                )
                if proc.returncode:
                    failures.append(kind + "_cleanup_inventory_failed")
                else:
                    remaining[kind] = len(proc.stdout.splitlines())
                    if remaining[kind]:
                        failures.append(kind + "_resources_remain")
            except Failure:
                failures.append(kind + "_cleanup_inventory_failed")
        self.result["owned_resources_remaining"] = remaining
        # root-owned 0600 HMAC files require no read to unlink from owned dirs.
        shutil.rmtree(self.private)
        self.result["cleanup"] = {"passed": not failures, "errors": failures}
        self.result["duration_s"] = round(time.monotonic() - self.started, 3)
        if failures:
            self.result["passed"] = False


def arguments():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--docker-host", default="unix:///tmp/kudurru-lab-docker.sock")
    parser.add_argument(
        "--app-sha", required=True, help="exact linker Commit expected in the images"
    )
    parser.add_argument("--backend-image", required=True)
    parser.add_argument("--edge-image", required=True)
    parser.add_argument("--vip-image", required=True)
    parser.add_argument(
        "--license-file",
        type=Path,
        default=ROOT / "internal/license/testdata/e2e-license.key",
    )
    parser.add_argument("--output", type=Path, required=True)
    return parser.parse_args()


def main():
    args = arguments()
    lab = Lab(args)
    for signum in (signal.SIGINT, signal.SIGTERM):
        signal.signal(signum, lambda *_: (_ for _ in ()).throw(Failure("interrupted")))
    try:
        lab.setup()
        lab.exercise()
        lab.result["passed"] = True
    except Failure as exc:
        lab.result.update(passed=False, error=str(exc))
    except Exception:
        lab.result.update(passed=False, error="unexpected_runner_failure")
    finally:
        try:
            lab.diagnostics()
        except Exception:
            lab.result["diagnostics_unavailable"] = True
        try:
            lab.cleanup()
        except Exception:
            lab.result.update(
                passed=False, cleanup={"passed": False, "errors": ["cleanup_exception"]}
            )
        args.output.parent.mkdir(parents=True, exist_ok=True)
        args.output.write_text(json.dumps(lab.result, indent=2) + "\n")
        print(json.dumps(lab.result, sort_keys=True))
    return 0 if lab.result["passed"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
