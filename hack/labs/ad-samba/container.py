"""Disposable Samba AD protocol checks; never a Windows acceptance result."""

import json
import os
from pathlib import Path
import re
import socket
import subprocess
import time
import traceback


REALM = "AD.LAB.TEST"
STATE = Path("/lab-state/domain")
CONFIG = STATE / "etc/smb.conf"
phase = "start"
diagnostics = []


def command(args, *, data=None, timeout=30, allow_failure=False):
    result = subprocess.run(args, input=data, text=True, capture_output=True,
                            timeout=timeout, check=False)
    if result.returncode and not allow_failure:
        # Tool output can contain generated credentials. Never echo it.
        raise RuntimeError(f"{Path(args[0]).name} failed (exit {result.returncode})")
    return result


def samba_tool(*args):
    # Keep Samba a separate executable. Its unused generated administrator
    # password is captured privately and discarded, never logged or passed
    # in process arguments. No Samba library is imported into this helper.
    result = command(["samba-tool", *args], timeout=60, allow_failure=True)
    if result.returncode:
        output = result.stdout + result.stderr
        diagnostics.extend(re.findall(r"\b(?:NT_STATUS|WERR|LDB_ERR)_[A-Z_]+\b", output))
        for known in ["Operation not permitted", "Permission denied", "No such file",
                      "MissingShareError", "no IPv4", "not found", "schema"]:
            if known.lower() in output.lower():
                diagnostics.append(known)
        raise RuntimeError("Samba lab provisioning command failed")


def ready(port, process, timeout=30):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if process.poll() is not None:
            raise RuntimeError("Lab service exited before readiness")
        try:
            with socket.create_connection(("127.0.0.1", port), timeout=0.2):
                return
        except OSError:
            time.sleep(0.1)
    raise RuntimeError("Lab service readiness deadline exceeded")


def stop(process):
    if process is None:
        return
    process.terminate()
    try:
        process.wait(timeout=5)
    except subprocess.TimeoutExpired:
        process.kill()
        process.wait(timeout=5)


def require_aes():
    # Configure disposable AD accounts explicitly; do not relax the probe's
    # AES-only admission when an account would otherwise receive RC4 tickets.
    for account in ["dns-probe", "member"]:
        change = (f"dn: CN={account},CN=Users,DC=ad,DC=lab,DC=test\n"
                  "changetype: modify\nreplace: msDS-SupportedEncryptionTypes\n"
                  "msDS-SupportedEncryptionTypes: 24\n-\n")
        command(["ldbmodify", "-H", str(STATE / "private/sam.ldb")], data=change)


def run():
    global phase
    os.umask(0o077)
    STATE.mkdir(mode=0o700)
    # Explicit lab-only Kerberos configuration; never use the host's realm.
    krb = STATE / "krb5.conf"
    krb.write_text(f"""[libdefaults]
 default_realm = {REALM}
 dns_lookup_kdc = false
 dns_lookup_realm = false
 dns_canonicalize_hostname = false
 rdns = false
 udp_preference_limit = 1
[realms]
 {REALM} = {{
  kdc = 127.0.0.1
 }}
""")
    os.environ.update(KRB5_CONFIG=str(krb), KRB5CCNAME=f"FILE:{STATE}/ccache",
                      KRB5_TRACE="/dev/null")
    phase = "provision"
    samba_tool("domain", "provision", f"--realm={REALM}", "--domain=LAB",
               "--server-role=dc", "--dns-backend=SAMBA_INTERNAL",
               f"--targetdir={STATE}", "--configfile=/dev/null", "--host-name=dc01",
               f"--host-ip={socket.gethostbyname(socket.gethostname())}", "--use-rfc2307",
               f"--option=posix:eadb = {STATE}/eadb.tdb")
    phase = "principals"
    for user in ["dns-probe", "member"]:
        samba_tool("user", "create", user, "--random-password", f"-s{CONFIG}")
    require_aes()
    samba_tool("spn", "add", "DNS/probe.ad.lab.test", "dns-probe", f"-s{CONFIG}")
    for principal, filename in [("DNS/probe.ad.lab.test", "probe"), ("member", "member")]:
        samba_tool("domain", "exportkeytab", str(STATE / f"{filename}.keytab"),
                   f"--principal={principal}", f"-s{CONFIG}")
    phase = "services"
    dc = probe = None
    try:
        dc = subprocess.Popen(["samba", "--foreground", "--no-process-group",
                               "--configfile", str(CONFIG)],
                              stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        ready(88, dc)
        phase = "kinit"
        command(["kinit", "-k", "-t", str(STATE / "member.keytab"), f"member@{REALM}"])
        phase = "probe"
        log = STATE / "probe.log"
        with log.open("w") as output:
            probe = subprocess.Popen(["/opt/lab/ad-dns-probe", "-listen", "127.0.0.1:15353",
                                      "-keytab", str(STATE / "probe.keytab"),
                                      "-principal", f"DNS/probe.ad.lab.test@{REALM}",
                                      "-zone", "probe.ad.lab.test", "-allow", "127.0.0.0/8"],
                                     stdout=output, stderr=output)
        ready(15353, probe)
        checks = []
        for transport in ["TCP", "UDP"]:
            phase = f"gss_tsig_{transport.lower()}"
            before = log.read_text().count("authenticated update refused")
            args = ["nsupdate", "-g", "-t", "5"]
            if transport == "TCP":
                args.append("-v")
            update = ("server probe.ad.lab.test 15353\nzone probe.ad.lab.test.\n"
                      "update add member.probe.ad.lab.test. 60 A 192.0.2.10\nsend\nquit\n")
            result = command(args, data=update, timeout=10, allow_failure=True)
            output = result.stdout + result.stderr
            signed_refusal = (result.returncode != 0 and "REFUSED" in output
                              and "tsig verify failure" not in output.lower())
            verified = log.read_text().count("authenticated update refused") == before + 1
            if not signed_refusal or not verified:
                for known in ["REFUSED", "SERVFAIL", "BADKEY", "tkey query failed", "tsig verify failure",
                              "Server not found", "KDC", "context established", "authenticated update refused",
                              "signature", "refused", "negotiation", "Kerberos", "GSSAPI", "timed out",
                              "probe requires AES tickets", "kerberos verification failed",
                              "ticket is for another service or realm", "probe requires AES context keys",
                              "probe requires optimistic Kerberos SPNEGO", "mutual authentication is required"]:
                    if known.lower() in (output + log.read_text()).lower():
                        diagnostics.append(known)
                raise RuntimeError(f"{transport} verified signed refusal check failed")
            checks.append({"case": f"samba_gss_tsig_{transport.lower()}", "result": "pass",
                           "authenticated": True, "signed_refusal": True,
                           "update_accepted": False})
        phase = "unsigned_update"
        before = log.read_text().count("authenticated update refused")
        unsigned = command(["nsupdate", "-t", "5"], data=update, timeout=10, allow_failure=True)
        if unsigned.returncode == 0 or "REFUSED" not in unsigned.stdout + unsigned.stderr:
            raise RuntimeError("Unsigned update did not receive a refusal")
        if log.read_text().count("authenticated update refused") != before:
            raise RuntimeError("Unsigned update acquired an authenticated observation")
        checks.append({"case": "unsigned_update_refused", "result": "pass",
                       "authenticated": False, "update_accepted": False})
        return {"result": "pass", "checks": checks,
                "samba": command(["samba", "--version"]).stdout.strip(),
                "nsupdate": command(["nsupdate", "-V"]).stdout.strip()}
    finally:
        stop(probe)
        stop(dc)


if __name__ == "__main__":
    try:
        print(json.dumps(run()))
    except Exception as exc:
        # Exception text from provisioning libraries can contain secrets.
        location = traceback.extract_tb(exc.__traceback__)[-1]
        print(json.dumps({"result": "fail", "phase": phase, "error_type": type(exc).__name__,
                          "location": f"{Path(location.filename).name}:{location.lineno}:{location.name}",
                          "diagnostics": diagnostics}))
        raise SystemExit(1) from None
