# Disposable Windows AD acceptance prompt

The owner currently has no Windows AD or two-VM keepalived lab. The v0.2 work
therefore uses isolated Linux Docker labs for the protocols they can exercise.
Windows Server container base images require a Windows host kernel and cannot
provide a supported AD Domain Services domain controller. Real Windows AD
acceptance needs full Windows VMs. The current Linux sandbox has no `/dev/kvm`
and insufficient free disk for the matrix; it is not the Windows runner.

The [Windows Server image](https://hub.docker.com/r/microsoft/windows-server)
is a container base image, not a bootable Windows VM. Microsoft's
[host requirements](https://learn.microsoft.com/en-us/virtualization/windowscontainers/deploy-containers/system-requirements)
require Windows for Windows containers, including Hyper-V isolation. Microsoft
also explicitly lists
[Active Directory Domain Services as unsupported in containers](https://learn.microsoft.com/en-us/virtualization/windowscontainers/quick-start/lift-shift-to-containers).
Running a Windows VM through a Docker wrapper still needs a hypervisor and
Windows installation media; it does not make that base image a Linux container.

Paste the following into a coding-agent session on an authorized disposable
Hyper-V/KVM/cloud Windows lab host, with this repository checked out:

```text
Provision and run the complete Kudurru v0.2 Windows AD DNS acceptance in a new,
isolated disposable lab. Read AGENTS.md, CLAUDE.md, CONTRIBUTING.md,
dev-docs/active-directory-dns.md, dev-docs/milestones/v0.2/completion-plan.md,
followup-audit.md and dev-docs/release-checklist.md first. Test the exact reviewed
PR #54 commit supplied by the owner; record its SHA and binary/image digests.
Do not deploy to production or use an existing organization domain.

First detect the available VM hypervisor, nested virtualization, disk, RAM and
Windows installation media. Use full supported Windows Server VMs, never a
Windows container as a domain controller. If media or a required VM primitive
is unavailable, report that specific prerequisite and do not claim acceptance.
Use only the disposable lab resources explicitly assigned to this session.

Create a private isolated virtual network and a single-domain forest
ad.lab.test. Select an unused RFC1918 subnet after checking host routes; do
not bridge it to the production LAN, forward DHCP outside it or use production
DNS. Assign these stable hostnames and roles:
- ad-dc01.ad.lab.test: first Windows Server domain controller, DNS and GC.
- ad-dc02.ad.lab.test: second Windows Server domain controller and GC.
- ad-member01.ad.lab.test: Windows domain member, initially unjoined.
- ad-dhcp01.ad.lab.test: Windows DHCP server with enabled name protection.
- kudurru01.ad.lab.test and kudurru02.ad.lab.test: Linux product DNS nodes.
- edge01.ad.lab.test and edge02.ad.lab.test: optional separate Linux VM VIP
  nodes for the original two-VM keepalived gate.
Use DHCP reservations/static lab addresses as appropriate, not production IPs.
Generate unique secrets into private files or the hypervisor's secret facility;
never print passwords, keytabs, cookies, tickets, credential caches or packet
traces. Commit automation/templates only, never generated credentials or VM
state. Do not change an existing host's domain membership or resolver settings.

Automate provisioning and teardown, including snapshots/checkpoints before
cutover. Establish the Windows DNS baseline, then exercise AD1 through AD12 and
every row of the acceptance matrix in dev-docs/active-directory-dns.md. Include:
1. Discovery/import of AD zones, delegations, application partitions, SRV,
   CNAME, A/AAAA/PTR, TTLs, timestamps and static/dynamic metadata. Surface
   unsupported permissions/policies as explicit blockers before any handoff.
2. Exact DNS service SPNs, keytab provisioning and rotation; Windows Netlogon,
   member and DHCP TKEY/GSS-TSIG registration. Exercise large PACs, TCP fallback,
   aliases/VIP routing, replay, invalid signatures, expired contexts and signed
   refusals. Unsigned updates must not acquire ownership.
3. Per-principal/record authorization, legitimate adoption of imported records,
   simultaneous registrations, DHCP shared credentials, DHCID/name protection,
   renew/release/expiry, IPv4/IPv6 and reverse cleanup. Preserve manual records.
4. Static protection and no-refresh/refresh/scavenge boundaries, durable aging
   and ownership metadata across restart, promotion, partition and rejoin.
5. Controlled shadow/final handoff with exact reconciliation. Stop Windows DNS
   on EVERY DC; independently confirm the services are stopped and that no
   product resolver forwards missing authoritative names to Windows DNS.
6. With Windows DNS still stopped: fresh member join, logon/Kerberos, DC/GC/PDC
   discovery and failover, Group Policy, replication, site/subnet changes,
   reboot/re-registration, DC promotion/demotion and DNS cleanup. Run dcdiag,
   repadmin, nltest and forced registrations with sanitized result summaries.
7. Roll back while accounting for every accepted change made after cutover.
   Reconcile registrations/ownership/aging into the selected Windows authority
   before restoring resolver settings. Restarting an obsolete DNS copy alone
   is not a successful rollback.
8. For edge01/edge02, exercise actual VRRP ownership, hard loss, readiness and
   stale-snapshot demotion, preemption policy, authenticated advertisements,
   ARP/neighbor convergence and bounded DNS interruption through each VIP.

Do not convert unavailable, unsupported or skipped behavior into a pass. The
current implementation has known open AD features: report exact failed gates
and implement fixes as separate signed-off conventional commits when authorized
by the repository task. Preserve original credentials, concrete transactions,
source ownership, writer fencing and final credential/license expiry; never
replay a refused mutation. Unit tests, Samba, MIT Kerberos and Linux container
checks are useful evidence but do not substitute for Windows behavior.

Produce a versioned sanitized report mapping each original F11–F15/F68/G16–G24
requirement to command, Windows build/role, exact product SHA, result and measured
recovery time. Link first-party scripts and retain private raw artifacts only in
the lab's excluded evidence directory. Record all failures and skipped cases.
Tear down only resources created by this run after collecting evidence. Do not
publish a release, merge the PR or change release branches.
```

Linux container results and this prompt do not close the real Windows or
original two-VM qualification gates. The completion tracker records those limits.
