### Added

- Zone groups for role bindings: label a zone `kudurru.io/zonegroup: <group>` and a RoleBinding (or `rbac.bindings` entry) with scope `{kind: zonegroup, name: <group>}` now grants its role on every zone of the group and on the record sets of those zones, on SQLite and Kubernetes. Zone lists show only member zones, `GET /zones` reports each zone's `zoneGroup`, `/auth/me` reports the permissions of the group grant, and API tokens may be scoped to a zone group. Removing the label takes the access away at once.

### Changed

- `GET /auth/me` reports `scopedPermissions` for every zone or zone-group grant, as the login answer does.
- Adding, changing or removing a zone's `kudurru.io/zonegroup` label needs `zones.write` at cluster scope; a zone admin of the zone or the group can no longer move it between groups.
- On Kubernetes, an administrator's API write that changes or removes a zone's `kudurru.io/zonegroup` label now takes the label away even when kubectl or a GitOps tool also set it: the write takes the zone object over like a whole-object replace. If the label is still not the requested one afterwards, the API answers 409 instead of 200.
- A RoleBinding with scope kind `zonegroup`, or an `rbac.bindings` entry with that scope, must name a valid label value (at most 63 letters, digits, `-`, `_` or `.`, starting and ending with a letter or digit). Such a binding granted nothing before 0.2.0; before you upgrade, rename it to the label value you will put on your zones or delete it. kudurru refuses to start with an invalid `rbac.bindings` entry, and a stored RoleBinding with an invalid name is refused on its next write.
- The settings help and the RoleBinding CRD reference no longer call the zonegroup scope reserved.
