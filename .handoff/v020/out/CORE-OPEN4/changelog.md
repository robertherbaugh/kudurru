### Fixed

- A zone-group member or a zone admin whose `PUT` of a zone was in flight
  while an administrator deleted that zone no longer creates the zone again
  (with its group label) on Kubernetes. Creating a zone always needs
  `zones.write` at cluster scope, and the write now decides this on the read
  it is compare-and-swapped against.
- Record set and zone lists, and `GET /auth/me`, answer `503` when the group
  of a zone they need cannot be read, instead of `200` with that zone's items
  or verbs left out.
- With `auth.kubernetes.mode: impersonate`, only the apiserver's own
  "cannot impersonate" refusal of kudurru's identity is reported as `503`
  (kudurru lacks the verb). A refusal for a principal whose name contains
  that phrase, or an admission webhook message that uses it, is the
  principal's `403` again.
- The Helm guide's kustomize impersonation example closes its code block, so
  the rest of the page renders again.

### Changed

- The admin guide and the access architecture page now say that every change
  of a zone's `kudurru.io/zonegroup` label through kudurru on Kubernetes
  (adding, changing or removing it) takes the whole zone object over and
  removes the fields that kubectl or a GitOps tool set and the change does
  not contain.

### Fixed (zones)

- A primary zone created under another primary zone no longer leaves the
  parent or the child answering SERVFAIL for good ("zone awaiting committed
  publication") when either zone's object changes (an edit of the parent, or
  either zone's own status update) just as the child is created.
