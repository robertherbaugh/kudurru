### Fixed

- On SQLite, a primary zone that receives dynamic updates no longer grows the node's database with every update: each content change now removes the zone data that no zone references any more, including data that earlier versions left behind (the Kubernetes backend still keeps it). Before, every signed UPDATE kept a full copy of the previous zone content, and the node took longer to publish each change, until sign-ins, reads and writes were refused with `503 generation_pending` while updates arrived.
- A node that commits steadily (signed UPDATEs, its own zone status work, sign-ins) publishes its changes promptly instead of starting its preparation over after every change, so the API stays usable while it receives dynamic updates.
- `Retry-After: 1` is no longer sent when part of the same request was already applied, and the API reference says what an `unavailable` answer without it means.
