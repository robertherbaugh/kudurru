# state.db of kudurru 0.1.0

The two files here are `state.db` as the released kudurru 0.1.0 binary wrote
it, gzip-compressed. 0.1.0 is the only release that 0.2.0 supports upgrading
from (decision D373), and these are its real bytes, not rows written by
today's code at an old schema number.

| File | Contents |
|---|---|
| `state.db.gz` | An ordinary installation: the first-boot defaults, a zone and record set from an objects directory, a zone, records, a forward zone, a block list and a client group created through the API, a record set with endpoints the zone cannot hold (an apex CNAME, an apex NS, a CNAME next to an A record) next to one it serves, five record sets that 0.1.0 reported Invalid (an `A` target that is not an address, two `A` endpoints for one name, an unknown record type, an `MX` without a preference, a CNAME and an `A` record for one name), a zone `dname.test.` with a record set of two DNAME endpoints for one name with different set identifiers (0.1.0 served both; 0.2.0 keeps the first), a block list whose URL was never fetched (0.1.0 published the failure on it), the default policy, personal allow and deny entries, the first-boot admin, three API tokens (break-glass, personal, bootstrap) and the audit trail of all of it. |
| `state-legacy.db.gz` | The same recipe plus what 0.1.0 accepted and 0.2.0 cannot serve: a secondary and a stub zone without their configuration, an `external` zone, a second DNSZone for the zone name `api.test.`, two primary zones whose update grants this release cannot compile (a `name` outside the zone with `update: deny`, an unknown type with `update: allow`) and a policy rewrite that has only a name. |

They are opened by `TestOpenReleasedV01Golden` and
`TestOpenReleasedV01LegacyGoldenIsRefused` (`../../golden_v01_test.go`) and
by `TestRunServeSQLiteUpgradeWithLegacyPlaceholders` (`cmd/kudurru`).

## Where they come from

`hack/upgrade-fixture.sh` (`make upgrade-fixture`) builds the 0.1.0 binary
from the `v0.1.0` tag (commit `89c59f899d7bfc061e697a563c25732f5ea97985`) and
runs the upgrade tests of `test/e2e`, which start `kudurru serve` of 0.1.0,
use it through its own API and admin socket, stop it, and copy the
`state.db` it left. The committed files are therefore the files those tests
upgrade. Every run produces new UIDs, timestamps and credentials; regenerate
only to change what the fixtures contain, and then adjust the object counts
in `golden_v01_test.go`.

Both files are at schema 1 with the tables `meta`, `objects`, `changes` and
`audit`, which the tests check before they open them.

## Credentials

The files hold only throwaway credentials of that test run: the admin
password hash is of the synthetic password `upgrade-e2e-password-1`
(`test/e2e/upgrade_test.go`), and the API tokens were random and are not
recorded anywhere. The node's encryption key is not part of `state.db`.

The files are named `.db.gz` because `*.db` is ignored by git. Tests unpack
them into a temporary directory and never open them in place.
