// Package unblock keeps the unblock requests users make on the block page
// (docs/deploy/blocking-page.md, "Unblock requests") as UnblockRequest
// objects in the store, so that every node's administrators see them in
// both store drivers.
//
// A Service watches the kind into memory: the block page looks up the
// status of a client's request for a name there on every page view and
// counts the pending requests for the cap without reading the store. Only
// Submit writes from the block page, and the caller bounds it (per-client
// and per-node rate limits); Submit refuses new requests once
// unblockRequests.maxPending are pending.
//
// A request's name is derived from the client (as querylog.privacy allows
// recording it) and the domain, so a repeated request of the same client
// for the same name updates the existing object instead of creating
// another; a request asked again after a decision returns to Pending and
// keeps the earlier decision in status.history.
//
// Approve adds the allow entry (the ui-allow list for everyone, the
// ui-allow-<group> list for one ClientGroup) and attaches the list to the
// policies concerned; Deny revokes an approval. The leader's janitor
// (RunJanitor) removes time-limited entries when they end, expires pending
// requests after unblockRequests.pendingExpiry and deletes decided ones
// after unblockRequests.retention. Every entry is linked to its request in
// status.allow, so removal survives restarts and removes only what an
// approval added.
package unblock
