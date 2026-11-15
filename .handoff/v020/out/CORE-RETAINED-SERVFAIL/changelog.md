### Fixed

- A node whose default upstream group is `mode: recursive` keeps answering
  cache-miss lookups while a configuration change is committed (an API token,
  any object write). Lookups that arrived in the tens of milliseconds after a
  commit used to be dropped, or answered SERVFAIL with extended DNS error 23
  (network error) although no authority had been asked; they are now resolved
  normally.
