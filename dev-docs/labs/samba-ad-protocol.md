# Disposable Samba AD protocol lab

This Linux Docker lab creates a Samba AD domain, two disposable user accounts,
an exact DNS service SPN, AES keytabs and a MIT Kerberos credential cache. BIND
`nsupdate` negotiates GSS-TSIG with Kudurru's separate feasibility probe over
TCP and UDP. Each case requires both a server-side authenticated observation
and a client-verified signed REFUSED response. A plain REFUSED response alone
cannot pass. The probe is outside the production binary and never accepts an
UPDATE.

Prerequisites: Linux Docker, Python 3.9+, and the Go version from the probe's
`go.mod`. Run from the repository root:

```sh
python3 hack/test-ad-samba.py --build
```

For this workspace's separate Docker daemon, whose package builds need host
networking:

```sh
python3 hack/test-ad-samba.py --build --build-network host \
  --docker-host unix:///tmp/kudurru-lab-docker.sock
```

Host networking applies only to the image build. Runtime creates an internal
Docker bridge and one container without published ports, host networking,
additional capabilities or host realm configuration. Samba, the probe and
`nsupdate` communicate within that container. Generated directory state,
passwords, tickets and keytabs live only in its private tmpfs. Docker logging
is disabled, commands carry file paths instead of credentials, and only the
sanitized first-party result is printed. The runner removes resources carrying
its unique ownership label even after failures; it never prunes other resources.
The development image remains available for another run.

Samba domain provisioning and DC identity changes run as root inside this
test container. An unprivileged UID trial failed during domain provisioning;
it is not a supported substitute for this fixture. The Dockerfile documents
the requirement, and the Semgrep/Trivy root-user exceptions apply only to this
test image. They do not exclude its dependencies or other security checks.

The test image aggregates unmodified Debian Samba, MIT Kerberos and BIND
packages. Their code is neither copied into this repository nor linked into
Kudurru. The Debian base digest is pinned; installed package versions and image
ID are recorded on each run. Rebuild to receive package security updates.

Results default to `.context/ad-samba-report.json`. They include commit,
source-change status, image/probe/helper hashes, tool versions, cases, elapsed
time and cleanup outcome. A failed case exits nonzero. No unavailable case is
counted as passed. The initial local run used Samba **4.22.11** and BIND
**9.20.30** and passed both transports; its dirty-source flag correctly showed
the not-yet-committed runner. Final evidence is recorded in the v0.2 gap ledger.

Samba accounts are explicitly configured for AES128/AES256 Kerberos. The first
attempt with Samba's default service-account encryption settings produced an
RC4 ticket, which the probe correctly refused. The lab configures its accounts;
it does not relax the probe's AES-only authentication requirement.

This checks the isolated probe against Samba-issued AD tickets. It does not
close production GSS-TSIG integration, record ownership, accepted RFC 2136
updates, Windows Netlogon/member/DHCP registration, large Windows PACs,
aging/scavenging, replication, VIP routing, cutover or rollback. Use the
[Windows lab prompt](windows-ad-acceptance-prompt.md) for that separate matrix.
Windows Server container images require a Windows host and cannot host AD DS;
the linked prompt includes Microsoft's documentation for both constraints.
