### Fixed

- A sign-in, a sign-out or a single whole-object write (without `If-Match`) that arrives while the node publishes its own work (zone status writes, signed UPDATEs) is no longer refused with `503 unavailable`: it waits up to five seconds for the newer publication and runs once on it. Every other command (writes with `If-Match`, deletes, `POST /apply` items, settings, imports, cluster and key commands) is still refused at once, because it is bound to what its request saw; nothing is applied in either case.
- On the object routes, `503 unavailable` carries `Retry-After: 1` only when no part of the request ran; an object write answered `unavailable` was never applied.
