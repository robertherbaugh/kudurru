### Fixed

- A command sent right after another command of the same client no longer
  fails intermittently with `503 unavailable` ("current writable serving
  generation unavailable"): a command whose own commit advanced the data no
  longer asks the node to publish the same data a second time, which
  retired the publication the next request was admitted on (D407). Seen as
  role binding, factor, password and local user commands answered 503
  instead of their real result.
- Adding a record with `POST /api/v1/zones/{zone}/records` right after a
  zone change no longer fails with `503 unavailable` when the node's own
  zone status write lands first: like a whole-object write, the record
  upsert is admitted again, once, on the newer publication (D399).
- An API command is no longer refused with `503 unavailable` when the node
  only published the same data again (a second publication of the same
  revision and dependencies, as a background wake produces): the command
  proceeds on the publication it was admitted on (D399).
