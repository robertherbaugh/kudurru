# Allowlist, Denylist and switching list entries off

Two ways to change what a block list does without editing it at its origin:

- the **Allowlist** and **Denylist** (dashboard: Filtering → Allowlist,
  Denylist, and Domains for both together): single names, wildcards and
  regular expressions you allow or deny, each with its own switch, comment,
  scope (everyone or client groups) and provenance, like Pi-hole's Domains
  page or Technitium's Allowed and Blocked zones;
- **switching single entries off** in any list, including lists imported
  from a URL or a ConfigMap (dashboard: the list's page → a source →
  Entries), like Pi-hole's per-domain switch but per list.

Switching an entry of an imported list off affects only that list: another
list that contains the same name still blocks it. To let a name through
everywhere, allow it. Both show on the Allowlist: switched-off entries of
block lists are listed there as **list exceptions** (and those of allow
lists on the Denylist), so the Allowlist is the one place that shows every
name kudurru lets through on purpose.

## Entry types

| Type | Value | Matches | Compiled rule |
|---|---|---|---|
| exact | `ads.example.com` | the name only | `ads.example.com` |
| wildcard | `*.example.com` (also accepted: `example.com`, `\|\|example.com^`) | `example.com` and every name below it | `*.example.com` |
| regex | an RE2 expression, with or without `/…/` | every name it matches | `/expression/` |

Names are lower-cased and IDNA-encoded; a trailing dot is dropped. An exact
value that starts with `*.` is refused (use wildcard). The query log names
the compiled rule and the list (`ui-allow`, `ui-deny`, ...).

## How allow and deny apply

An allow match wins over every block list of the client's policies (and
over blocked services); a deny entry blocks like a block list, unless an
allow list of the client's policies allows the name.

The entries are stored on BlockLists, one per scope, as structured entries
of the source `ui` (`spec.sources[].entries`):

| Scope | List | Referenced from |
|---|---|---|
| Allow, everyone | `ui-allow` | `allowListRefs` of every policy of the namespace kudurru may edit |
| Deny, everyone | `ui-deny` | `blockListRefs` of every policy of the namespace kudurru may edit |
| Allow, one ClientGroup | `ui-allow-<group>` (in the group's namespace) | `allowListRefs` of the policies whose `clientGroupRefs` name the group |
| Deny, one ClientGroup | `ui-deny-<group>` | `blockListRefs` of those policies |

An entry scoped to several groups has one copy in each group's list; the
dashboard and the API show and edit it as one entry. The lists are created
on demand, managed by the API, and deleted (with their references) when
their last entry goes. A group no policy covers cannot be chosen: its
clients get the `default` policy, which other clients share. A group list
applies through the group's policies, so a policy that covers several
groups applies it to all of them.

The leader keeps these references (every 30 seconds and on every
Allowlist or Denylist change): a policy created after the entries gets the
everyone lists, a policy that starts naming a group gets the group's lists,
and one that stops naming the group loses them. Policies managed by a file
(objects.d) or an integration are not edited: the dashboard and
`GET /api/v1/allowlist` (`unattached`) name them; add the list to their
`allowListRefs` / `blockListRefs` in the file.

A structured entry:

```yaml
apiVersion: kudurru.io/v1alpha1
kind: BlockList
metadata: { name: ui-allow, namespace: default }
spec:
  mode: allow
  sources:
    - name: ui
      entries:
        - value: cdn.example.com
          type: exact
          comment: needed by the video player
          created: "2026-10-11T12:00:00Z"
          createdBy: user:alice
          origin: querylog          # manual, querylog, unblock, api or migrated
        - value: "*.example.net"
          type: wildcard
          enabled: false            # kept, not applied
        - value: '^ads?[0-9]*\.'
          type: regex
          origin: unblock
          request: default/ur-0123456789abcdef   # the approved UnblockRequest
```

Other lists may use `entries` too (`kubectl apply`, objects.d); only the
`ui-*` lists appear on the Allowlist and Denylist pages.

Where entries come from: the Add dialog of the pages (`manual`), the query
log's Allow and Deny buttons (`querylog`, exact, everyone), approved unblock
requests (`unblock`, with the request; see
[blocking-page.md](blocking-page.md#unblock-requests)), the API and
`kudurru lists allow|deny` (`api`), and the migration below (`migrated`).
Every change writes an audit event (`allowlist.add`, `.update`, `.delete`,
`.batch`; `denylist.*`) and flushes the names of exact and wildcard entries
from the node's response cache; every node recompiles its lists, which also
flushes the local edge cache.

## Switching entries of any list off

`spec.sources[].exclude` lists rules of that source that are not compiled.
Each value is a rule as the source's format normalises it, which is how the
entries browser lists them: `ads.example.com` for the hosts line
`0.0.0.0 ads.example.com` (one value per host of a line), `||ads.example.com^`,
`/expression/`. The exclusions are part of the list's spec, so they survive
refreshes; an exclusion that no longer matches anything in the fetched body
(the list dropped the name, or the value is not written the way the format
normalises it) is reported in `status.sources[].staleExcludes` (first 50)
rather than silently kept. `status.sources[].excluded` counts the entries
switched off and `status.sources[].entries` the ones compiled, so the
dashboard shows "N of M entries enabled".

```yaml
spec:
  sources:
    - url: https://example.org/hosts.txt
      exclude: ["tracker.example.com", "ads.example.com"]
```

For structured entries the switch is `entries[].enabled`; switching it in
the entries browser edits that.

The entries of a URL or ConfigMap source are read from the node's list
cache: the leader fetches the body and every node pulls it by digest. A node
that has no body yet answers 503 ("the fetched body ... is not on this node
yet"); `stale: true` means the node still serves an older body than the
published digest.

### List exceptions on the Allowlist and Denylist

Every exclusion of a block list is a row of the Allowlist, every exclusion
of an allow list a row of the Denylist (the ui lists themselves excepted):
kind `exception`, origin `list`, the list and source it lives in, the
policies that use the list, and the other lists that still match the name
(`stillMatchedBy`; the exception does not change them). It has a switch and
nothing else: on means the entry is switched off in its list (the
exception is in effect); switching it off, or deleting it, removes the
exclusion, so the list's entry applies again. The Add dialog's scope
"Only in list …" creates exceptions: `POST /api/v1/allowlist` with
`{values, list, source?}` switches the entries of that list (in every
source that has the name, or in `source`) off; a name the list does not
contain is refused per value. Filters: `kind=entry|exception`,
`scope=list:<ns/name>`; `q` also matches the list name.

File- and Kubernetes-managed lists are read-only through the API (edit the
file or the object). The starter list (`starter`, seeded on first boot,
managed by `bootstrap`) can be tweaked: like any API edit of a first-boot
default (decision D84), switching an entry off takes the list over (managed
by the API from then on), so a later objects.d declaration of `starter` is
reported as a conflict instead of silently dropping your exclusions.

## Checking disabled rules

**Is it allowed?** displays disabled rules returned by the same built-in query
that produced the verdict. The server uses the question's captured lists,
client identity and selected policies; the browser does not fetch a later list
or run its own regular expressions. A disabled rule does not mean the whole
list allows the name: another source or rule can still block it. The displayed
verdict remains the actual pipeline result.

`POST /api/v1/dns/query` includes optional `exclusions` entries with `list`,
`source` (zero-based compiled source index, excluding RPZ sources), `rule` and
`effect` (`block`, `allow`, `exception` or `rewrite`). These entries require
`blocklists.read` in addition to the query's `stats.read`; otherwise the normal
query result omits them. Only actual source rules removed by an exclusion or
disabled structured entry are reported. Stale exclusions, unselected lists,
disabled filtering and rules after an earlier authoritative/local answer do
not contribute evidence. This describes the original question; response-address
and alias-target exclusions are outside this diagnostic.

Evidence is bounded: compilation retains at most 1,024 distinct source/rule
pairs and 1 MiB of rule text per list, at most 4,096 bytes per rule and 64 regex
or contextual patterns. Each diagnostic inspects at most 4,096 lists/rules and
returns at most 64 matches. `exclusionsTruncated: true` means a bound, unsupported
excluded pattern or cancellation made the evidence incomplete. The UI says
some matching disabled rules may be omitted. These limits affect explanations
only; filtering still excludes every configured matching source rule. Ordinary
DNS traffic does not perform the diagnostic scan. Older servers show the
verdict without exclusion details.

## Migration from 0.1 lists

Before structured entries, the Domains page, the query log and unblock
approvals wrote plain lines into an inline source `ui` of the same lists
(`example.com`, `||example.com^`, `/re/`). The leader converts every such
list it may edit once at start (`domainlist-migration`), and every write to
a list converts it too: each line becomes an entry (`||x^` and `*.x` become
wildcard `*.x`) with the list's creation time and origin `migrated`, or
`unblock` with the request when an approval in force holds the line. Lines
that are none of these rules (an `@@` exception, a hosts line with several
names) stay in an inline source `ui-unconverted`, so nothing is lost. Lists
managed by a file are read in both forms and never rewritten. Approvals made
before the migration still remove their entry when they end.

## API

| Route | |
|---|---|
| `GET /api/v1/allowlist`, `/denylist` | `?namespace=&kind=entry\|exception&type=exact\|wildcard\|regex&enabled=true\|false&scope=everyone\|<ns/group>\|list:<ns/name>&q=&limit=&continue=`; items (entries and list exceptions), totals, the lists, unattached policies |
| `GET /api/v1/domains` | both, `?action=allow\|deny` and the same filters |
| `POST /api/v1/allowlist`, `/denylist` | `{values[], type?, namespace?, groups?, comment?, enabled?, origin?}`, or `{values[], list, source?}` for list exceptions; refused values in `processed.errors` |
| `GET`, `PATCH`, `DELETE /api/v1/allowlist/{id}` | PATCH `{enabled?, comment?, value?, type?, scope?: everyone\|groups, groups?}`; a list exception takes only `{enabled}` |
| `POST /api/v1/allowlist:batch` | `{ids[], op: enable\|disable\|delete}` |
| `GET /api/v1/blocklists/{ns}/{name}/sources/{source}/entries` | every source type; `?q=&enabled=&limit=&continue=`; items `{value, line, enabled}` |
| `PATCH …/entries` | `{enable?, disable?}` for every source type, `{add?, remove?}` for inline sources |
| `POST /api/v1/domains/{allow\|deny}/{exact\|regex}` | compatibility: writes everyone entries |

Reading needs `blocklists.read`, writing `blocklists.write`.
