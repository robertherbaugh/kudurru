# HA-OPEN5 decision rows

The package was assigned D381, which HA-UPDATE-ROUTING already introduced, and
it also changes D375. Both complete rows are in this directory and replace
the earlier ones as a whole:

- `decision-D381.md`: replaces `out/HA-UPDATE-ROUTING/decision.md` (D381) and
  supersedes the follower half of CI-RED-2's SERVFAIL statement.
- `decision-D375.md`: replaces `out/CORE-APPLY-MULTI/decision-D375.md` (D375).

No new decision number is used. The self-service changes (answer for the
original principal; named cause of `committed_output_unavailable`) stay
within D368 and need no row change; the ledger describes them.
