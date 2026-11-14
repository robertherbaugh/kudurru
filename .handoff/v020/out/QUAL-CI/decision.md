# QUAL-CI decision row (no number assigned)

The package prompt assigned no decision number. The row below records the one
product decision of the review-fix pass (commit `49aae9ee`, `fix(dns): answer an
upstream failure across a canonical commit`). It is written without a number:
the integrator assigns the next free `D<nnn>`. No code comment or document in
the commits cites a number for it. If the integrator decides against the
change, drop `49aae9ee` together with `0048c606` (a rewrap of the paragraph that
`49aae9ee` extends in `docs/architecture/query-path.md`): `49aae9ee` carries the
guard change, its unit rows, the new end-to-end test and the restored form of
`TestCentralQuerylogTwoNodes`, and the tree without the two commits has the
first-pass, reviewed form of that test (a fake resolver instead of the discard
port). No other commit of the package depends on them.

| D<unassigned> | Upstream-failure refusals across canonical commits | D332's retained class also covers the empty SERVFAIL of an ordinary query whose upstream failed or timed out (`servfail_upstream`, `timeout_upstream`): response code SERVFAIL, AA clear, empty answer and authority sections, an additional section that holds at most the OPT record, and a pipeline origin that is unattributed or failure. It keeps every check of the retained path: the original dependency installations, the anchor input, the one finite output attempt and no transfer continuation. A failure response that claims authoritative, local, mixed or synthesized provenance, any failure that carries records, and DNSSEC validation failures (`dnssec_bogus`) stay bound to the full data generation, as before. | After every canonical commit, queries that were captured on the retired data generation finish under D332's retained check. A forwarded answer was delivered there, but the failure of the same forwarding path was refused at output (`serving: dependency changed`), so the client got no answer and waited for its own timeout instead of the immediate SERVFAIL that lets it try its next resolver. D332's own rationale names dropped data-free refusals as the defect it fixed, and the empty unsynced SERVFAIL is already retained on the same grounds: the message carries no zone, cache or proof data and needs no data-generation credit. The first hosted-style run of `TestCentralQuerylogTwoNodes` exposed it (an API token created through the admin socket is such a commit). | Leaving the refusal dropped and routing the test around it (a silent client timeout after each commit on a node whose upstream is failing); retaining every SERVFAIL regardless of status or origin (would add validation failures and data-claiming origins without review); deciding by wire shape alone (provenance has to come from the pipeline, never from the message); answering from the successor publication or retrying the query (renews a lifetime, which D332 rejects) |
