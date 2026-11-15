"""First-party wire and result checks; no Docker daemon required."""

import importlib.util
from pathlib import Path
import socket
import struct
import unittest
from types import SimpleNamespace
from unittest.mock import Mock

SPEC = importlib.util.spec_from_file_location(
    "vip_lab", Path(__file__).with_name("run.py")
)
lab = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(lab)


def name(value):
    return (
        b"".join(bytes([len(p)]) + p.encode() for p in value.rstrip(".").split("."))
        + b"\0"
    )


def response(owner=b"\xc0\x0c", question=lab.QNAME, address=lab.ANSWER, flags=0x8400):
    return (
        struct.pack("!6H", 123, flags, 1, 1, 0, 0)
        + name(question)
        + struct.pack("!HH", 1, 1)
        + owner
        + struct.pack("!HHIH", 1, 1, 60, 4)
        + socket.inet_aton(address)
    )


class AnswerTest(unittest.TestCase):
    def test_exact_compressed_and_uncompressed_owner(self):
        for owner in (b"\xc0\x0c", name(lab.QNAME), name(lab.QNAME.upper())):
            lab.validate_answer(response(owner=owner), 123)

    def test_wrong_question_owner_or_address_rejected(self):
        for packet in (
            response(question="other.lab.invalid."),
            response(owner=name("other.lab.invalid.")),
            response(address="203.0.113.69"),
        ):
            with (
                self.subTest(packet_length=len(packet)),
                self.assertRaisesRegex(lab.Failure, "wrong_dns_answer"),
            ):
                lab.validate_answer(packet, 123)

    def test_unavailable_and_non_authoritative_not_success(self):
        for flags in (0x8402, 0x8600, 0x0400, 0x8000):
            with self.subTest(flags=flags), self.assertRaises(lab.Failure):
                lab.validate_answer(response(flags=flags), 123)
        with self.assertRaises(lab.Failure):
            lab.validate_answer(response(), 124)

    def test_truncation_and_compression_cycles_rejected(self):
        for packet in (
            b"",
            response()[:20],
            response()[:-1],
            response(owner=b"\xc0\xff"),
            response()[:12] + b"\xc0\x0c" + response()[14:],
        ):
            with self.subTest(length=len(packet)), self.assertRaises(lab.Failure):
                lab.validate_answer(packet, 123)

    def test_tcp_short_read(self):
        class Conn:
            parts = [b"a", b"b", b""]

            def recv(self, _count):
                return self.parts.pop(0)

        with self.assertRaisesRegex(lab.Failure, "short_dns_tcp"):
            lab.recv_exact(Conn(), 3)


class MonitorTest(unittest.TestCase):
    def test_success_gap_spans_failure_and_case_boundary(self):
        monitor = lab.DNSMonitor({"dns": "127.0.0.1"})
        monitor.samples = [
            (9.9, "dns", "udp", "ok"),
            (10.1, "dns", "udp", "transport"),
            (10.3, "dns", "udp", "wrong_dns_answer"),
            (11.2, "dns", "udp", "ok"),
        ]
        result = monitor.summary(10, 12)["dns/udp"]
        self.assertEqual(result["max_success_gap_s"], 1.3)
        self.assertEqual(result["failures"], 2)
        self.assertEqual(result["wrong_answers"], 1)

    def test_no_samples_not_silent_success(self):
        monitor = lab.DNSMonitor({"dns": "127.0.0.1"})
        result = monitor.summary(10, 12)["dns/tcp"]
        self.assertEqual(result["samples"], 0)
        self.assertEqual(result["max_success_gap_s"], 2)


class OwnershipTest(unittest.TestCase):
    def fixture(self, secondary_state="BACKUP"):
        runner = lab.Lab.__new__(lab.Lab)
        runner.vips = {"dns": "192.0.2.53"}
        runner.active = {"a": True, "b": True}
        runner.owners = Mock(return_value={"dns": ["a"]})

        def request(node, _port, path, _json=True):
            if path == "/metrics":
                return 'kudurru_vip_master{vip="dns"} ' + str(int(node == "a"))
            return {
                "ready": True,
                "state": {
                    "vips": {
                        "dns": {"state": "MASTER" if node == "a" else secondary_state}
                    }
                },
            }

        runner.request = Mock(side_effect=request)
        return runner

    def test_duplicate_interface_owner_cannot_pass(self):
        runner = self.fixture()
        runner.owners.return_value = {"dns": ["a", "b"]}
        self.assertIsNone(runner.steady_snapshot())

    def test_faulted_peer_cannot_prove_nopreempt_recovery(self):
        runner = self.fixture("FAULT")
        self.assertEqual(runner.steady_snapshot(), {"dns": "a"})
        self.assertIsNone(runner.steady_snapshot(recovered=True))
        self.assertEqual(self.fixture().steady_snapshot(recovered=True), {"dns": "a"})

    def test_fifo_and_metrics_must_agree_with_interface(self):
        runner = self.fixture("MASTER")
        self.assertIsNone(runner.steady_snapshot())
        runner = self.fixture()
        previous = runner.request.side_effect
        runner.request.side_effect = lambda n, p, path, j=True: (
            'kudurru_vip_master{vip="dns"} 0'
            if path == "/metrics"
            else previous(n, p, path, j)
        )
        self.assertIsNone(runner.steady_snapshot())


class CleanupTest(unittest.TestCase):
    def test_mismatched_name_or_label_never_deleted(self):
        runner = lab.Lab(SimpleNamespace(app_sha="fixture"))
        runner.resources = [
            ("volume", runner.prefix + "-valid"),
            ("container", "unrelated"),
            ("network", runner.prefix + "-wrong-label"),
        ]
        calls = []

        def docker(*args, **_kwargs):
            calls.append(args)
            out = b""
            if args[1] == "inspect":
                out = (
                    "someone-else"
                    if args[-1].endswith("wrong-label")
                    else runner.run_id
                ).encode()
            return SimpleNamespace(stdout=out, returncode=0)

        runner.docker = docker
        runner.cleanup()
        removed = [c[-1] for c in calls if c[1] == "rm"]
        self.assertEqual(removed, [runner.prefix + "-valid"])
        self.assertFalse(runner.result["cleanup"]["passed"])
        self.assertEqual(
            runner.result["cleanup"]["errors"],
            ["ownership_mismatch", "ownership_mismatch"],
        )

    def test_cleanup_attempts_other_resources_after_command_timeout(self):
        runner = lab.Lab(SimpleNamespace(app_sha="fixture"))
        runner.resources = [
            ("volume", runner.prefix + "-valid"),
            ("container", runner.prefix + "-timeout"),
        ]
        removed = []

        def docker(*args, **_kwargs):
            if args[-1].endswith("timeout"):
                raise lab.Failure("command_timeout")
            if args[1] == "rm":
                removed.append(args[-1])
            return SimpleNamespace(
                stdout=runner.run_id.encode() if args[1] == "inspect" else b"",
                returncode=0,
            )

        runner.docker = docker
        runner.cleanup()
        self.assertEqual(removed, [runner.prefix + "-valid"])
        self.assertFalse(runner.result["cleanup"]["passed"])

    def test_unavailable_inventory_is_not_successful_cleanup(self):
        runner = lab.Lab(SimpleNamespace(app_sha="fixture"))
        runner.docker = Mock(return_value=SimpleNamespace(stdout=b"", returncode=1))
        runner.cleanup()
        self.assertFalse(runner.result["cleanup"]["passed"])
        self.assertEqual(len(runner.result["cleanup"]["errors"]), 3)


if __name__ == "__main__":
    unittest.main()
