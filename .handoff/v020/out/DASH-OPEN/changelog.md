### Fixed

- Dashboard: a session without `cluster.read` no longer requests `GET /cluster` every 30 seconds (each refused read was written to the audit log as a denial on every node); it follows the node's role through the public `GET /auth/status`, and a `403` ends the poll for any session and is not retried on navigation or window focus. The Nodes page no longer reads `GET /cluster` without `cluster.read`; its Cluster panel says so instead.
- Dashboard: the invitation of the "Invite a member" wizard can be closed with the close button, Escape or the backdrop (after the "Close the invitation?" prompt) also when the primary's answer took a moment; before, only "Done" closed it.
- Dashboard: an `?add=` link to the Allowlist or Denylist no longer opens the add dialog briefly on a follower before the node's role is known, and the link is dropped from the address when the session cannot follow it.
- Dashboard: on a follower, only a forwarded change (not a sign-in, a DNS query from Tools or a change the follower runs itself) schedules the second read of the page six seconds later.
- Dashboard: the audit log renders a page of 100 events several times faster: its cells no longer each carry a right-click menu (rows still expand with a click, the chevron, Enter or Space).
