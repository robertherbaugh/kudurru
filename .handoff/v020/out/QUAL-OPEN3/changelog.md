No user-facing change: QUAL-OPEN3 changes only two opt-in live test fixtures. If the integrator wants a line for the release notes:

### Fixed

- The live dnsdist edge tests (`hack/test-dnsdist-live.sh`) now wait for the backend node to finish committing a zone change before they send their single-shot DNS queries, so all six legs pass reliably on the pinned upstream dnsdist image.

Integrator note: if the owner accepts shipping the post-commit answer window unchanged (D383), the release notes should say so. The brief's groups are Added, Changed and Fixed, so this suggested line is for a "Known issues" note next to `[0.2.0]` rather than a bullet under those groups:

- After any configuration change is committed, a node can leave queries for its authoritative zones unanswered for a fraction of a second (up to about 0.6 s measured; up to about 0.9 s, partly SERVFAIL, when the change touches that zone or its records), and a forward zone whose own settings changed can answer SERVFAIL for up to about 0.6 s. Clients that retry recover. A fix is planned after v0.2.0.
