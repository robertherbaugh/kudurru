### Fixed

- RPZ feeds apply from a just-started node's first answer: a BlockList the
  policy references but the list resolver has not compiled yet still selects
  its feeds, so a persisted copy is enforced, or the query fails closed, instead
  of a listed name resolving for a short window after a restart. This holds on
  Kubernetes for either `blocking.allowDegraded` value. The SQLite runtime
  compiles every list with the configuration it serves, so it never had this
  window; its RPZ selector takes the same pending lists for consistency.
- With `blocking.allowDegraded: false`, a Kubernetes node that starts from its
  `local.db` snapshot now reports ready only once that snapshot is published
  and every referenced list is in the policy; it was ready about 100 ms early.
- A restart while the apiserver is unreachable reuses the persisted copy of an
  unsigned RPZ feed (now covered by a test); signed feeds still fail closed
  until the pod restarts with the apiserver reachable.
- `kudurru lists rpz` and `/info/messages` no longer show the serial, counts
  and ignored owners of an RPZ copy the node discarded during acceptance next
  to "no usable copy".
- `kudurru import coredns`: `cache.minTTL` is always set and never above
  `cache.maxTTL` (`cache 3` and `success 100 10 3600` produced settings that
  failed validation); oversized `prefetch` and `serve_stale` values are omitted
  with a loss line, and `serve_stale 0` says it turns stale answers off.
- `kudurru import coredns` follows CoreDNS routing more closely: server blocks
  keyed by a classless reverse prefix (such as `10.0.0.0/12`) shadow the PTR
  answers they cover, a block whose key cannot be read stops `hosts` and
  `rewrite` answers from being imported, and a block on another port no longer
  shadows names on port 53 (the report says kudurru answers every listener
  alike). When blocks on different ports answer the same name, the block on
  the lowest port wins and the report names the omitted port. Encrypted
  classless keys (such as `tls://10.0.0.0/12`) are reported like other
  encrypted keys, and the report says when a block with unreadable keys
  may take imported conditional routes.
