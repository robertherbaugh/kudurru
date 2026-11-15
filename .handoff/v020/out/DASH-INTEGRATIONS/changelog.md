### Added

- Dashboard: an Integrations page (System → Integrations) lists every Integration with the state the host reports for its stored revision, its deletion policy, last sync and manager, and links to a page per instance.
- Dashboard: "New integration" is a wizard built from the node's provider manifests: the provider types of the build (a type the node does not run, such as a webhook on Kubernetes, is shown with the reason and cannot be chosen), the provider's settings from its configuration schema without the fields the node refuses (an optional group such as a Cloudflare zone's `adoptFrom` or the UniFi `inventory` is required to be complete only when one of its fields is set), and one credential reference (a granted file or environment name, never a value) per credential the provider reads.
- Dashboard: an instance's page runs Check, Preview and "Publish this plan" (a sync bound to the reviewed plan's digest), "Sync now" for inventory providers and the provider's advertised actions; an expired or stale preview is shown as refused with nothing published, a preview made before the instance changed is withdrawn with a "Preview superseded" notice, and after a sync the provider did not acknowledge the page shows the outcome as unknown and offers no further sync of that instance until a Check or a Preview succeeds (the open page holds this; a reload does not).
- Dashboard: an instance's configuration is edited in place (bound to the stored revision), deleted with a confirmation that explains its deletion policy, and while terminating shows what is left, what blocks and what is uncertain, with "Release without cleanup" for `integrations.release`.

### Changed

- Dashboard: controls that release an Integration are disabled in static mode, as the API refuses them there.
