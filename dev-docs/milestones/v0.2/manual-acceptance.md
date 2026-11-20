# v0.2.0 manual release acceptance

These are **manual release gates** for [#133](https://github.com/robertherbaugh/kudurru/issues/133),
documented under [#168](https://github.com/robertherbaugh/kudurru/issues/168).
The scheduled routine and agents may run synthetic tests, but must not conduct
human acceptance, handle production keys, or record manual sign-off. Follow the
[release checklist](../../release-checklist.md) for artifact verification and
the [existing evidence](final-acceptance.md) for the scope of earlier tests.

## Human UI acceptance matrix

Run against the **exact final release artifacts only**, including the dashboard
served by them, not a dev server, mock SPA, prior snapshot, or separate rebuild
of the same source. Verify the trusted artifact checksums first. For each
distinct artifact under review, record the release version, asset filename and
platform, its full SHA256, the SHA256 of the executable if extracted from an
archive, and the test browser, version, and viewport. Use a new matrix if the
bytes change. The human approver fills in every result, approver, UTC date
(`YYYY-MM-DD`), and exact full artifact SHA256; leave unrun rows blank. A
blocked flow is not a pass. Agent screenshots, browser automation, and
synthetic test logs never count as human approval.

Merging the code or this runbook is **not** human UI acceptance. After a human
has run the matrix, a human reviewer may explicitly approve the completed
matrix in an evidence PR. The approval must identify the matrix and the exact
final artifact SHA256 values it covers. Merging that evidence PR retains the
record of the approval; the merge itself is not the test or the approval. A
new build with different bytes requires a new run and approval, even from the
same source. Publication of byte-identical artifacts does not change what was
approved.

Use an isolated disposable deployment, synthetic names and DNS data, and
separate admin and viewer browser sessions. Use an entitled final artifact for
the RBAC and integrations rows and for creating an additional Policy; do not
mistake a license lock for a permission refusal. Prepare a primary zone, two
test client groups (and clients that match them), a controllable RPZ transfer
source, and an inventory-capable test integration with credentials provisioned
privately by reference. Use standalone SQLite for the mutable IAM and membership
flows. Check layout, focus, labels, validation messages, and feedback after
save/reload in each flow. Strip private data from notes; keep credentials,
one-time tokens, invite codes, and raw screenshots out of the acceptance record.

| Page / flow | Steps | Expected result | Pass / Fail | Approver | UTC date | Final artifact SHA256 |
| --- | --- | --- | --- | --- | --- | --- |
| DNS → Zones (`ZonesPage`, `ZoneDetailPage`) | Open a disposable primary zone from the list. Add, edit, and delete a disposable A record; reload after each save and query the test name. | Type, readiness, and source are legible. The record lifecycle persists across reloads and DNS reflects each change. | | | | |
| Zone detail → views (`ZoneDetailPage`) | In **Add record**, select **Served to → Client group**. Save a distinct A record for that group and query from matching and nonmatching clients. Remove the view record. | The row identifies the view and links to the group. Only the matching client receives the view record; removing it leaves the baseline unchanged. The served-record list is not proof that a **Published only (public)** record is missing. | | | | |
| System → Access → Identity (`AccessPage`) | As admin on SQLite, create a disposable identity, bind the viewer role, and confirm the resulting access in a separate session. Exercise a service-account token and revoke it without recording the secret. | Identity and binding reload correctly. The bound identity receives its intended access; token material is shown only on creation and stops working after revocation. | | | | |
| Filtering → Lists → RPZ | Add a disposable list with **Add an RPZ feed**, a **Policy zone**, and test **Primary servers**. Open the list detail before and after its first transfer. | **RPZ feeds on this node** shows pending before transfer and then serving on that node with the expected serial and rule counts. | | | | |
| Filtering → Policies → RPZ | Attach the transferred list to a disposable Policy for the test client group. Inspect the policy detail; query a blocked name and an allowed control name. | Priority, group, list reference, and readiness match the fixture. The blocked name gets the configured response; the control still resolves. A list without an applied policy is not a DNS proof. | | | | |
| System → Integrations (`IntegrationsPage`) | Open a disposable provider integration using a private credential reference. Run **Preview**, verify planned changes against the test fixture, then **Publish this plan** and **Check**. | The UI shows the plan/digest and status, publishes only the intended fixture changes, and never renders the credential value. | | | | |
| Filtering → Inventory | Seed a synthetic device through an inventory integration. Search and exercise Source, Kind, and Staleness filters; follow the reporting integration link. | A device can appear without ever querying DNS. The fixture's address state, tags, freshness, and source are correct; filters and links work. | | | | |
| System → Nodes → membership (`NodesPage`) | Inspect the node, health, and membership panels. On a separate disposable standalone SQLite node, use **Activate cluster**, restart if requested, then **Invite a member**. | The membership and role after restart agree with the activation result; the invite is shown once. Do not do this on an existing cluster or treat it as multi-node failover acceptance. | | | | |
| Read-only refusals (built-in `viewer`) | In the viewer session, open Zones, Lists, and Policies; try their write controls. Open the admin-only Integrations, Access, and Nodes routes directly using URLs copied from the admin session. Try Inventory too. | Readable data still loads; writes are unavailable and the UI explains missing write permissions (for example `zones.write`). Restricted pages refuse access without exposing protected data. The built-in viewer does not have `inventory.read`, `integrations.read`, or `cluster.read`; this is not a read-only view of every admin page. | | | | |

Remove fixture identities, tokens, invites, and provider changes or destroy the
isolated deployment after review. If a flow fails, record only the sanitized
symptom and rerun it on the verified final artifact after resolution.

## Production-key expiry and grace

**Owner decision (November 20, 2026):** Prefer the issued-key procedure below in
a disposable VM. If that run is not feasible, the owner accepts the
[clock-controlled synthetic fallback](#synthetic-fallback-for-expiry-and-grace-only)
for expiry/grace behavior only. This documents the available paths; it does
not record that either path has passed.

**Authorized owner only. Do not run in CI, a Conductor workspace, on the host,
or on an existing deployment.** Use a disposable Linux VM with an isolated
network, no shared folders, backups, snapshots after key import, or log shipping.
Provision Bun, a reviewed checkout (for the checker only), and the exact final
archive before introducing the key. Verify the archive against trusted release
checksums, extract it without replacing or rebuilding the executable, and
record both the archive and binary SHA256. A development or `licensetest`
build cannot prove the production trust root. The binary has no release clock
override.

1. Using the authorized production portal at **real UTC**, open **Licenses →
   Issue license**. Use an authorized disposable test recipient and written
   Enterprise test agreement. Under **More plans**, select Enterprise with
   one node and at least branding and audit. Set **Expires on (UTC)** to
   **two UTC calendar days after issuance**. Leave customer email delivery
   off, then **Sign and issue**.
   The portal rejects dates before tomorrow UTC and requires more than 24 hours
   of validity. `expiresOn` maps to `23:59:59Z` of that date; the signed `exp`
   is authoritative. Record only the issuance outcome and real UTC date, not
   the licensee, license ID, signing material, or signature. Do not give the
   VM any signing credentials.
2. Transfer the issued key privately into the disposable VM, into an
   owner-only directory (`0700`) outside all checkouts, with a regular file
   readable only by its owner (`0600`). Run as that owner, not root. Never
   paste the key into a command, shell history, environment variable, ticket,
   or log. Disable guest NTP (`sudo timedatectl set-ntp false` on a systemd
   guest) **and** hypervisor guest time synchronization. Confirm the guest
   clock is still at real UTC and disconnect external networking before
   starting the checker. Keep two VM consoles open.
3. With tracing off and a private output location, run the
   [checker](../../../hack/production-license-check.ts) from the checkout in
   the first VM console. Pass the private **path**, not the key. Set the
   expected binary SHA256 from the executable extracted from the verified
   archive. For example, with `BINARY`, `BINARY_SHA256`, `PRIVATE_KEY_FILE`, and
   `PRIVATE_RESULT_FILE` set to the corresponding paths/hash (use a new file
   for the result, never the key path):

   ```sh
   (
     umask 077
     set -C
     bun --no-env-file hack/production-license-check.ts sequence "$BINARY" "$BINARY_SHA256" \
       "$PRIVATE_KEY_FILE" > "$PRIVATE_RESULT_FILE"
   )
   ```

   The checker starts the archive executable on loopback with disposable SQLite
   data, verifies `active`, the effective signed entitlement, branding, audit,
   an additional Policy, and an authoritative DNS answer, then restarts it on
   the same state and verifies persistence. It keeps the restarted process
   running through both clock jumps. It discards the process logs and prints
   only a sanitized result on success. Do not retrieve raw `/api/v1/license`
   responses, report the key path, or run `kudurru license status` into evidence.
4. At the first prompt, in the **second disposable VM console only**, set guest
   UTC to **signed expiry + 1 day**. On a GNU/Linux guest, use
   `sudo date -u --set="@<Unix seconds printed in the prompt>"` (replace the
   placeholder with the number). Verify with `date -u` and press Enter at the
   checker prompt. Expect `grace`, `graceEndsAt = exp + 30 × 24 hours`, the
   same features, limits, branding, audit and Policy access as `active`, and
   the same DNS answer. The checker requires confirmation within one guest
   hour of the target. Do not restart the node or replace the key to force the
   transition.
5. At the next prompt, set guest UTC the same way to **signed expiry + 31 days**
   (not 31 days after the grace test) and confirm. Expect `expired` with
   Community limits (one node, one Policy, no licensed features), default
   branding, audit and new Policies refused, the existing Policy retained, and
   the same DNS answer. The API may still name the original plan; the effective
   features and limits are what matter.
   The checker then starts a **fresh data directory with no key**, expecting
   `community` (not `expired`), the same Community limits/default branding,
   audit refused, and DNS answering. No change to production DNS is involved.
6. Only the owner records pass/fail, approver, the **real** UTC run date,
   authorization/issuance outcome, final asset filename/platform and archive
   and binary SHA256, along with reviewed, sanitized results for each phase.
   Phase timestamps are **guest** times, not the approval date. Record the
   cleanup outcome. A zero exit status alone does not establish issuance,
   artifact provenance, or human visual approval. On failure, keep the gate
   open and report the sanitized failing stage only; do not save process logs,
   keys, raw claims, licensee names, license IDs, or signatures. Remove the test
   key and destroy the isolated VM and its disposable storage even on failure.
   Never reconnect a clock-advanced guest to a live network.

### Synthetic fallback for expiry and grace only

Use the exact source revision that produced the final release artifacts. Run
the clock-controlled tests from that checkout with the project's Go toolchain;
they use synthetic keys and injected test clocks, not the guest or host clock.
No production key, signer, portal access, or release archive is an input.

```sh
go test -count=1 -v ./internal/license -run '^(TestLifecycle|TestBrandingOriginalValidityAndGrace)$'
go test -count=1 -v ./cmd/kudurru -run '^(TestServingBrandingActualHTTPPlansWritesAndGrace|TestServingBrandingEntitlementDoesNotChangeNativeDNS)$'
```

These tests check the active-to-grace and grace-to-expired boundaries,
continued entitlements in grace, Community limits after expiry, the branding
gate on the serving HTTP fixture, and authoritative DNS over loopback UDP and
TCP with the expired entitlement. These are source-built test binaries and
fixtures, not a final archive process left running through guest clock jumps.

Label the evidence **Synthetic (injected clock) — expiry/grace only**. Record
the source commit, its link to the final artifact build provenance, Go version,
OS/architecture, exact commands, real UTC test date, and sanitized results.
Do not use the final archive SHA256 as an identity for the executed test
binaries. Record this fallback separately from issued-key runtime evidence;
it does **not** prove the final archive's live expiry transition, production
trust or signing, authorized portal issuance, or human UI acceptance. The
release record must cite the applicable production trust/signing and issuance
evidence separately. The checker's `active` and `community` modes are local
smoke checks without transitions; they are not the clock-controlled fallback.
