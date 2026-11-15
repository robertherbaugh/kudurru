### Changed

- Refusals that waiting does not change no longer carry `Retry-After: 1`: the Kubernetes answers `cli_login_unavailable`, `provider_logout_unavailable`, `factor_management_unavailable`, `user_management_unavailable`, `sessions_unavailable`, `external_users_unavailable` and the last-fallback `unavailable` of the authentication routes, and the `unavailable` answers of cluster enrollment, invitations and promotion on a node that cannot have them (static, standalone, Kubernetes). Status codes and keys are unchanged.

### Fixed

- Every `503` that a supported deployment answers now has a hint that says what to do: the Kubernetes session, authenticator, external-account and local-user refusals, cluster enrollment, invitations and promotion on a static or standalone node, an unknown or unready OIDC provider for browser and command-line login, DHCP reads on a node without DHCP, the VIP event stream on a node without a VRRP address, and integration administration on Kubernetes.
- Every `501 not_implemented` now has a hint, and so do the `503 unavailable` answers for a server without the block page renderer or the DHCP setup manager, for query suggestions, status and actions without query history, and for cluster-wide query actions without a central database.
