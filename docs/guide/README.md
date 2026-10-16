# kudurru guide

This guide tells you how to use kudurru and how to run it. It has two parts.

| Part | Read it if you | Start here |
|---|---|---|
| User guide | use a network that kudurru serves | [User guide](user-guide.md) |
| Administrator guide | install, set up and run kudurru | [Administrator guide](admin/README.md) |

The text follows the rules of Simplified Technical English (ASD-STE100).
The rules are in [Writing rules](style.md).

## What kudurru is

kudurru is a DNS server. A DNS server changes the name of a site into a
number that a device can use. kudurru can also refuse a name. The
administrator of the network decides which names it refuses.

kudurru has these functions:

- It answers queries for the zones that you own.
- It sends all other queries to upstream DNS servers, or it finds the
  answers itself from the root servers and checks DNSSEC.
- It blocks names that are on block lists.
- It applies different rules to different client groups.
- It has a dashboard that shows what happens on the network.
- It can assign IPv4 addresses with an optional DHCPv4 server on Linux with the SQLite store.

## Parts of this guide

| Chapter | Content |
|---|---|
| [User guide](user-guide.md) | The block page, how to ask for access, and the dashboard for viewers |
| [1. Overview](admin/01-overview.md) | Terms, roles and the layout of the dashboard |
| [2. Install and first sign-in](admin/02-install-and-sign-in.md) | Start kudurru, sign in and point the network at it |
| [3. Watch the network](admin/03-watch-the-network.md) | Dashboard, query log and long-term statistics |
| [4. Block lists and domains](admin/04-lists-and-domains.md) | Lists, the Allowlist and the Denylist |
| [5. Groups, clients and policies](admin/05-groups-clients-policies.md) | Decide which rules apply to which device |
| [6. Access requests](admin/06-access-requests.md) | The block page and the answers to users |
| [7. Zones and upstreams](admin/07-zones-and-upstreams.md) | Your own DNS names and the external servers |
| [8. System pages](admin/08-system.md) | Nodes, settings, tools and diagnosis |
| [9. People, roles and tokens](admin/09-people-and-tokens.md) | Who can sign in and what they can do |
| [10. Command line](admin/10-command-line.md) | The `kudurru` command |
| [11. Maintenance](admin/11-maintenance.md) | Back up, restore, upgrade and recover |
| [12. Troubleshooting](admin/12-troubleshooting.md) | Problems and their solutions |
| [13. License and keys](admin/13-license.md) | License terms, feature entitlements and installing a key |

## How to read this guide

- Each chapter starts with **Before you start**. It lists the role and the
  items that you need.
- Numbered steps are procedures. Do the steps in the order that they have.
- A line that starts with `CAUTION:` tells you about a risk. Read it
  before you do the step.
- **Bold** text is text that you see on the screen. `Code` text is text
  that you type, a file name or a value.
- The screenshots show demo data. The names and numbers on your system are
  different.

## Version

This guide describes kudurru v0.1.0 ("stone"), dated 16 October 2026.
The [changelog](../../CHANGELOG.md) lists the release changes and known
limitations. Some functions are for later versions. The guide says so
where it matters.

## More information

| Subject | Document |
|---|---|
| All deployment shapes (Docker, Kubernetes, packages) | [Deploying kudurru](../deploy/README.md) |
| Backup, key rotation and upgrade runbooks | [Runbooks](../deploy/runbooks.md) |
| The design of kudurru | [Architecture](../architecture.md) |
| Every setting | [Settings reference](../../site/content/reference/configuration.md) |
| Every command | [Command reference](../../site/content/reference/cli.md) |
| Move from Pi-hole | [Coming from Pi-hole](../../site/content/migrate/pihole.md) |
| Move from AdGuard Home | [Coming from AdGuard Home](../../site/content/migrate/adguard-home.md) |
| Move from Technitium | [Coming from Technitium](../../site/content/migrate/technitium.md) |
| Switch DNS servers | [Cutover and rollback checklist](../../site/content/migrate/cutover.md) |
