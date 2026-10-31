# Require HTTPS for list and catalog downloads

Set the host configuration on every node that can download external data:

```yaml
blocking:
  httpsOnly: true
```

The equivalent environment variable is `KUDURRU_BLOCKING__HTTPSONLY=true`.
The default is false, preserving existing HTTP sources. This setting is
host-only and requires a restart; the API cannot disable it.

With the setting enabled, new external BlockList, blocked-services catalog
and optional AdGuard services downloads require HTTPS on the initial request
and every redirect. An HTTP target is rejected before it is contacted, even
when an HTTPS server redirects to it. The BlockList source status or catalog
refresh status reports `download policy: HTTPS is required by
blocking.httpsOnly`. This refusal is not a transient download failure and
BlockList downloads do not retry it within the same attempt. Normal scheduled
refreshes remain available after an operator fixes the source.

Certificate verification remains enabled. Private certificate authorities must
be trusted by the node's normal TLS trust store. Existing proxy configuration,
request timeouts and local-address restrictions still apply. In particular,
HTTPS does not grant access to local addresses that
`blocking.allowLocalListURLs` otherwise refuses.

This setting governs new external downloads. It preserves previously cached
or published data and authenticated internal copies from peers; it does not
retroactively establish how that data was downloaded. Enable it consistently
on every fetching node. If historical transport assurance is required, replace
existing caches during a planned restart and obtain new data from HTTPS
origins. Inline rules, ConfigMaps and the embedded catalog do not use external
HTTP and remain available. A rejected refresh keeps the last good body and
does not silently erase current filtering data.
