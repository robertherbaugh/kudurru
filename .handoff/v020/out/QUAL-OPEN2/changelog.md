# CHANGELOG.md `[0.2.0]` bullets from QUAL-OPEN2 (D373)

These add to the bullets of `out/QUAL-UPGRADE-OPEN/changelog.md`. One
bullet there needs a change: the `Duplicated` report of a record set at a
dynamically owned name is now on the record set itself, not only in the
zone's source diagnostics (see the first Fixed bullet below).

### Changed

- **A start no longer says "settings applied" for cluster keys of
  `kudurru.yaml` that are not in effect (SQLite).** After a node's first 0.2
  start the cluster settings object (`Kudurru/default`) owns the cluster
  settings, so a later edit of a cluster key in the file changes nothing.
  For differing keys that apply without a restart, the start now logs the
  warning `cluster settings in kudurru.yaml are not in effect`, with those
  keys and the stored values now in effect (list and object values are
  left out; `kudurru settings get` shows them). Differing keys that need a
  restart are named in a second warning, `cluster settings in kudurru.yaml
  differ from the cluster settings object`, which says the node keeps the
  values it started with instead of claiming the stored value. Later
  cluster changes are still logged as `settings applied`.

### Fixed

- **A record set at a name that a dynamic update owns now says so in its own
  conditions (SQLite).** Each endpoint left out because its name holds a
  record a dynamic update (RFC 2136) added is named as `Duplicated` in the
  record set's conditions: `Degraded` while other endpoints are served, and
  not `Ready` when none is. Before, the record set could still say it was
  fully ready.
- Checking the static records of a primary zone against its dynamic records
  no longer slows down with many dynamic DNAMEs: a zone at the
  100,000-record-set limit is checked in time proportional to its size. The
  leader no longer repeats the record set check for a zone whose record
  sets and committed data did not change.
