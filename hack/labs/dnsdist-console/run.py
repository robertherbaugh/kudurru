#!/usr/bin/env python3
"""Qualify console mutations on a disposable YAML-started dnsdist 2.1.3.

Runs dnsdist as a separate executable. No upstream implementation is embedded.
Only fixed result labels are printed; credentials and child output stay private.
"""

import argparse
import base64
from contextlib import ExitStack
import hashlib
import json
import os
from pathlib import Path
import secrets
import shutil
import socket
import socketserver
import ssl
import struct
import subprocess
import tempfile
import threading
import time
import uuid

DEFAULT_IMAGE = "powerdns/dnsdist-21:2.1.3@sha256:c70ae7363712188ab2878d22bc9f1801cae29190688474a224caee2089f74837"
NAME = b"\x07console\x03lab\x07invalid\x00"
QUESTION = NAME + struct.pack("!HH", 1, 1)


class Failure(Exception):
    """A fixed, credential-free diagnosis."""


def require(value, label):
    if not value:
        raise Failure(label)


def read_exact(conn, size):
    data = b""
    while len(data) < size:
        part = conn.recv(size - len(data))
        if not part:
            raise Failure("short_dns_frame")
        data += part
    return data


def response(query, address):
    require(len(query) >= 12, "short_backend_query")
    # This disconnected fixture accepts a single uncompressed A/IN question.
    offset = 12
    for _ in range(128):
        require(offset < len(query), "bad_backend_question")
        length = query[offset]
        offset += 1
        require(length < 64 and offset + length <= len(query), "bad_backend_label")
        if length == 0:
            break
        offset += length
    require(query[offset:offset + 4] == b"\x00\x01\x00\x01", "bad_backend_type")
    question = query[12:offset + 4]
    return (query[:2] + struct.pack("!5H", 0x8400, 1, 1, 0, 0) + question
            + b"\xc0\x0c" + struct.pack("!HHIH", 1, 1, 0, 4)
            + socket.inet_aton(address))


class TCPHandler(socketserver.BaseRequestHandler):
    def handle(self):
        self.request.settimeout(3)
        try:
            while True:
                size = struct.unpack("!H", read_exact(self.request, 2))[0]
                wire = response(read_exact(self.request, size), self.server.answer)
                self.request.sendall(struct.pack("!H", len(wire)) + wire)
        except (Failure, OSError):
            return


class UDPHandler(socketserver.BaseRequestHandler):
    def handle(self):
        data, sock = self.request
        try:
            sock.sendto(response(data, self.server.answer), self.client_address)
        except (Failure, OSError):
            return


class TCPServer(socketserver.ThreadingTCPServer):
    daemon_threads = True
    block_on_close = False


class Backend:
    def __init__(self, answer):
        self.tcp = TCPServer(("127.0.0.1", 0), TCPHandler)
        self.port = self.tcp.server_address[1]
        self.udp = socketserver.ThreadingUDPServer(("127.0.0.1", self.port), UDPHandler)
        self.udp.daemon_threads = True
        self.tcp.answer = self.udp.answer = answer
        for server in (self.tcp, self.udp):
            threading.Thread(target=server.serve_forever, daemon=True).start()

    def close(self):
        for server in (self.tcp, self.udp):
            server.shutdown()
            server.server_close()


def query(port, expected, tls=None):
    ident = secrets.randbits(16)
    wire = struct.pack("!6H", ident, 0x100, 1, 0, 0, 0) + QUESTION
    if tls is not None:
        tls.sendall(struct.pack("!H", len(wire)) + wire)
        reply = read_exact(tls, struct.unpack("!H", read_exact(tls, 2))[0])
    else:
        with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as conn:
            conn.settimeout(2)
            conn.connect(("127.0.0.1", port))
            conn.send(wire)
            reply = conn.recv(4096)
    require(len(reply) >= 12, "short_dns_reply")
    rid, flags, qd, an, _, _ = struct.unpack("!6H", reply[:12])
    require(rid == ident and flags & 0x8000 and flags & 15 == 0 and qd == an == 1,
            "dns_header_mismatch_rcode_" + str(flags & 15))
    require(reply[12:12 + len(QUESTION)] == QUESTION and
            reply[-4:] == socket.inet_aton(expected), "dns_answer_mismatch")


class Lab:
    def __init__(self, args):
        self.args = args
        self.root = Path(tempfile.mkdtemp(prefix="dnsdist-console-", dir=args.work_dir))
        # nosemgrep: python.lang.security.audit.insecure-file-permissions.insecure-file-permissions -- Private directory: only its owner may traverse it; 0644 would expose names and prevent owner traversal.
        os.chmod(self.root, 0o700)
        self.log = (self.root / "child-output.private.log").open("wb")
        os.chmod(self.log.name, 0o600)
        self.name = "kudurru-console-" + uuid.uuid4().hex[:12]
        self.backends = []
        self.reservations = []
        self.created = False
        self.results = []
        self.observations = []

    def command(self, argv, timeout=20, check=True):
        try:
            result = subprocess.run(argv, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                    timeout=timeout, check=False)
        except subprocess.TimeoutExpired:
            raise Failure("child_timeout") from None
        self.log.write(result.stdout + result.stderr)
        self.log.flush()
        if check:
            require(result.returncode == 0, "child_command_failed")
        return result

    def docker(self, *args, **kwargs):
        return self.command(["docker", "--host", self.args.docker_host, *args], **kwargs)

    def port(self):
        sock = socket.socket()
        sock.bind(("127.0.0.1", 0))
        self.reservations.append(sock)
        return sock.getsockname()[1]

    def console(self, statement):
        # Key remains inside the mode-0600 config, never in argv or the report.
        result = self.docker("exec", self.name, "dnsdist", "-C", "/lab/dnsdist.yml",
                             "-e", statement, check=False)
        require(result.returncode == 0, "console_exit_failed")
        # dnsdist can return exit zero after a Lua error; a final marker is needed.
        require(b"KUDURRU_CONSOLE_OK" in result.stdout, "console_marker_missing")

    def cert(self, index):
        stem = self.root / ("certificate-" + str(index))
        self.command(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes",
                      "-days", "1", "-subj", "/CN=console.lab.invalid",
                      "-addext", "subjectAltName=DNS:console.lab.invalid", "-set_serial", str(index),
                      "-keyout", str(stem) + ".key", "-out", str(stem) + ".crt"])
        key, crt = Path(str(stem) + ".key"), Path(str(stem) + ".crt")
        os.chmod(key, 0o600)
        return key, crt

    def install_cert(self, pair):
        for src, name in zip(pair, ("tls.key", "tls.crt")):
            candidate = self.root / (name + ".candidate")
            shutil.copyfile(src, candidate)
            os.chmod(candidate, 0o600)
            candidate.replace(self.root / name)

    def tls(self, port):
        # Trust only the two generated lab certificates, then also verify exact DER.
        context = ssl.create_default_context(cadata=self.trusted_certificates)
        conn = context.wrap_socket(socket.create_connection(("127.0.0.1", port), 2),
                                   server_hostname="console.lab.invalid")
        conn.settimeout(3)
        return conn

    def fingerprint(self, pair):
        der = ssl.PEM_cert_to_DER_cert(pair[1].read_text())
        return hashlib.sha256(der).digest()

    def run(self):
        self.docker("image", "inspect", self.args.image)
        version = self.docker("run", "--rm", "--network", "none", "--cap-drop", "ALL",
                              "--security-opt", "no-new-privileges", "--entrypoint", "dnsdist",
                              self.args.image, "--version")
        require(b"2.1.3" in version.stdout + version.stderr, "unexpected_dnsdist_version")
        self.backends = [Backend("203.0.113.1"), Backend("203.0.113.2")]
        dns_port, tls_port, console_port = self.port(), self.port(), self.port()
        first_id, second_id = str(uuid.uuid4()), str(uuid.uuid4())
        cert1, cert2 = self.cert(1), self.cert(2)
        self.trusted_certificates = cert1[1].read_text() + cert2[1].read_text()
        self.install_cert(cert1)
        config = {
            "acl": ["127.0.0.1/32"],
            "console": {"listen_address": f"127.0.0.1:{console_port}",
                        "key": base64.b64encode(secrets.token_bytes(32)).decode(),
                        "acl": ["127.0.0.1/32"]},
            "binds": [{"listen_address": f"127.0.0.1:{dns_port}", "protocol": "Do53"},
                      {"listen_address": f"127.0.0.1:{tls_port}", "protocol": "DoT",
                       "tls": {"certificates": [{"certificate": "/lab/tls.crt", "key": "/lab/tls.key"}],
                               "minimum_version": "tls1.2"}}],
            "backends": [{"address": f"127.0.0.1:{self.backends[0].port}", "id": first_id,
                          "name": "initial", "protocol": "Do53",
                          "health_checks": {"mode": "up"}}],
            "load_balancing_policies": {"default_policy": "firstAvailable",
                                        "servfail_on_no_server": True},
        }
        path = self.root / "dnsdist.yml"
        path.write_text(json.dumps(config))  # JSON is a YAML subset.
        os.chmod(path, 0o600)
        (self.root / "dnsdist.lua").write_text("-- No Lua configuration directives.\n")
        for sock in self.reservations:
            sock.close()
        self.reservations.clear()
        self.docker("run", "--detach", "--name", self.name, "--label", "io.kudurru.console-lab=" + self.name,
                    "--network", "host", "--user", f"{os.getuid()}:{os.getgid()}",
                    "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--read-only",
                    "--mount", f"type=bind,src={self.root.resolve()},dst=/lab,readonly",
                    "--entrypoint", "dnsdist", self.args.image,
                    "--supervised", "--disable-syslog", "-C", "/lab/dnsdist.yml")
        self.created = True
        deadline = time.monotonic() + 15
        while True:
            try:
                query(dns_port, "203.0.113.1")
                break
            except (Failure, OSError):
                if time.monotonic() >= deadline:
                    self.docker("logs", self.name, check=False)
                    raise Failure("yaml_start_failed") from None
                time.sleep(0.1)
        pid = self.docker("inspect", "--format", "{{.State.Pid}}", self.name).stdout
        self.results.append("yaml_start_and_dns")
        # A Lua failure is not reliably reflected in the client process exit code.
        refused = self.docker("exec", self.name, "dnsdist", "-C", "/lab/dnsdist.yml", "-e",
                              'error("intentional_console_lab_refusal"); return "KUDURRU_CONSOLE_OK"',
                              check=False)
        require(b"KUDURRU_CONSOLE_OK" not in refused.stdout, "failed_command_acknowledged")
        query(dns_port, "203.0.113.1")
        self.results.append("failed_command_has_no_acknowledgment_or_dns_change")
        with ExitStack() as connections:
            held = connections.enter_context(self.tls(tls_port))
            require(hashlib.sha256(held.getpeercert(binary_form=True)).digest() == self.fingerprint(cert1),
                    "initial_certificate_mismatch")
            query(tls_port, "203.0.113.1", held)
            self.console(f'newServer({{address="127.0.0.1:{self.backends[1].port}", id="{second_id}", '
                         'name="added", healthCheckMode="up"}):setUp(); return "KUDURRU_CONSOLE_OK"')
            self.console(f'getServer("{first_id}"):setDown(); return "KUDURRU_CONSOLE_OK"')
            self.docker("exec", self.name, "dnsdist", "-C", "/lab/dnsdist.yml", "-e", "showServers()")
            query(dns_port, "203.0.113.2")
            self.results.append("new_server_udp")
            with self.tls(tls_port) as fresh:
                query(tls_port, "203.0.113.2", fresh)
            self.results.append("new_server_fresh_tls")
            try:
                query(tls_port, "203.0.113.2", held)
                self.observations.append("held_tls_connection_reselected_new_backend")
            except Failure as exc:
                if str(exc) != "dns_header_mismatch_rcode_2":
                    raise
                self.observations.append("held_tls_connection_returned_servfail_after_selected_backend_set_down")
            # Certificate acceptance uses an independently established connection
            # after membership changes, so it tests certificate context retention.
            held.close()
            held = connections.enter_context(self.tls(tls_port))
            self.results.append("new_server_and_set_down_live_dns")
            self.console(f'rmServer("{first_id}"); return "KUDURRU_CONSOLE_OK"')
            query(dns_port, "203.0.113.2")
            self.results.append("remove_server_live_dns")
            self.install_cert(cert2)
            self.console('reloadAllCertificates(); return "KUDURRU_CONSOLE_OK"')
            with self.tls(tls_port) as fresh:
                require(hashlib.sha256(fresh.getpeercert(binary_form=True)).digest() == self.fingerprint(cert2),
                        "rotated_certificate_mismatch")
                query(tls_port, "203.0.113.2", fresh)
            query(tls_port, "203.0.113.2", held)
            self.results.append("certificate_rotation_and_held_connection")
            (self.root / "tls.crt").write_text("intentionally invalid certificate\n")
            # Upstream may signal error in output with exit zero. Check the live result,
            # and do not report this as an accepted certificate mutation.
            self.docker("exec", self.name, "dnsdist", "-C", "/lab/dnsdist.yml", "-e",
                        'reloadAllCertificates()', check=False)
            with self.tls(tls_port) as fresh:
                require(hashlib.sha256(fresh.getpeercert(binary_form=True)).digest() == self.fingerprint(cert2),
                        "invalid_certificate_replaced_good_certificate")
                query(tls_port, "203.0.113.2", fresh)
            query(tls_port, "203.0.113.2", held)
            self.results.append("invalid_certificate_retains_previous_tls_context")
            self.install_cert(cert2)
            require(self.docker("inspect", "--format", "{{.State.Pid}}", self.name).stdout == pid,
                    "unexpected_child_restart")
            self.results.append("same_child_pid")
            held.close()
        # Reconcile the persisted YAML and prove the next process starts with the
        # intended member after the in-memory console updates.
        config["backends"][0].update(address=f"127.0.0.1:{self.backends[1].port}", id=second_id, name="added")
        path.write_text(json.dumps(config))
        self.docker("restart", "--time", "3", self.name)
        deadline = time.monotonic() + 15
        while True:
            try:
                query(dns_port, "203.0.113.2")
                break
            except (Failure, OSError):
                if time.monotonic() >= deadline:
                    raise Failure("restart_convergence_failed") from None
                time.sleep(0.1)
        self.results.append("persisted_yaml_restart_convergence")

    def close(self):
        if self.created:
            self.docker("logs", self.name, check=False)
            label = self.docker("inspect", "--format",
                                '{{index .Config.Labels "io.kudurru.console-lab"}}', self.name).stdout
            require(label.strip().decode() == self.name, "cleanup_ownership_mismatch")
            self.docker("rm", "--force", self.name)
        for sock in self.reservations:
            sock.close()
        for backend in self.backends:
            backend.close()
        self.log.close()
        if self.args.keep_private_log:
            dst = self.args.result.with_suffix(".private.log")
            shutil.copyfile(self.root / "child-output.private.log", dst)
            os.chmod(dst, 0o600)
        shutil.rmtree(self.root)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--docker-host", default=os.environ.get("DOCKER_HOST", "unix:///var/run/docker.sock"))
    parser.add_argument("--image", default=DEFAULT_IMAGE)
    parser.add_argument("--work-dir", type=Path, default=Path(".context"))
    parser.add_argument("--result", type=Path, required=True)
    parser.add_argument("--keep-private-log", action="store_true")
    args = parser.parse_args()
    args.work_dir.mkdir(parents=True, exist_ok=True)
    lab = Lab(args)
    report = {"image": args.image, "scope": "dnsdist 2.1.3 YAML console capability; not supervisor acceptance"}
    try:
        lab.run()
        report["status"] = "pass"
    except Failure as exc:
        report.update(status="fail", failure=str(exc))
    except Exception:
        report.update(status="fail", failure="unexpected_lab_error")
    finally:
        report["passed"] = lab.results
        report["observations"] = lab.observations
        lab.close()
    args.result.parent.mkdir(parents=True, exist_ok=True)
    args.result.write_text(json.dumps(report, indent=2) + "\n")
    print(json.dumps(report, indent=2))
    return 0 if report["status"] == "pass" else 1


if __name__ == "__main__":
    raise SystemExit(main())
