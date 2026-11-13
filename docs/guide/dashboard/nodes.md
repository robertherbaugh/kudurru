# Check node and cluster health

**Before you start**

- You need the `admin` role.
- You need to know which nodes belong to this deployment.

## Inspect the deployment

1. Open **System** > **Nodes**.
2. Check **Ready** for each node.
3. Compare **Version** across the nodes.
4. Check **Zones** and **Snapshot age** for stale configuration.
5. Click **Health** for a node that needs investigation.

![The Nodes page with readiness, versions, leader information and cluster status.](../images/nodes.png)

*Figure 1. The page distinguishes the node serving your dashboard from the cluster leader.*

The leader handles jobs that run once, such as list refreshes.
Another node takes over if the leader stops.
This leader is separate from the DHCP serving replica.

**Cluster** identifies the store, leader, epoch and object write availability.
**Edge** reports dnsdist, the floating address and backends when configured.
Use [Diagnosis](diagnosis.md) for linked configuration problems and [Tools](tools.md) for API health checks.

## Run an enrolled SQLite cluster

On a SQLite node, **Cluster membership** shows this node's enrollment: its
mode (`standalone`, `primary` or `follower`), the cluster ID and the member
ID. The button next to the title depends on the mode. Each wizard reads the
current state when it opens and sends exactly what you confirmed: if you read
the state again or untick "I reviewed", confirm again before you finish. It
sends its command once, and you cannot close the dialog until it answers: if the reviewed state changed (`409`) or the answer was lost or
says that the outcome is unknown, it sends nothing more; close it, check the
enrollment and decide again. The commands need the `admin` role.

- **Activate cluster** (a dormant standalone node): review the enrollment,
  give the member name and the internal address (`host:port` of the internal
  listener, port 8082 by default) and a reason, then activate. Restart
  kudurru on that node afterwards; until then followers cannot join.
- **Invite a member** (the primary): review the enrollment, give the new
  member's name and internal address, how long the invitation is valid (2, 5
  or 9 minutes, counted on your computer's clock; the primary accepts at most
  ten on its own clock) and a reason, then issue it. A `409` refusal means
  that the primary's state changed, that the name or address is already used
  or reserved by an earlier member, or that the expiry is too far ahead or
  already past on the primary's clock; start a new wizard. The next screen shows the
  invitation once, with the `kudurru cluster join --offline` command and the
  Docker Compose changes for the new node. Copy the invitation over a private
  channel; closing the dialog drops it and it is not shown again. If the join
  fails, follow
  [Recover an incomplete join](../../deploy/sqlite-join.md#recover-an-incomplete-join)
  instead of issuing a second invitation.
- **Review promotion** (a follower): the wizard shows whether the follower
  is eligible (its primary unreachable for two pull intervals) and why not.
  Type the node's name and a reason to promote it. Promote one follower only.

**Root trust anchors** shows this node's managed root trust-anchor state
(`kudurru cluster anchors`): the owner and its keys, the updater and the root
check, the last pull from the primary, and every state that needs you, with
a link to what to do. That includes a member that refuses the primary's
trust-anchor state and a member fenced behind its term. Every member shows
its own state; open the dashboard of each member. A fenced or conflicted
member closes its API, so its dashboard shows only that it is fenced (or in
conflict) and links the procedure; read its `kudurru_root_anchor_*` metrics
or stop it and run `kudurru anchors status --offline`.

[Enrolled SQLite cluster](../../deploy/sqlite-cluster.md) describes every step
with the CLI.

## Check the result

All expected nodes are ready and use the intended version.
If a node is unavailable, treat combined statistics as partial until it recovers.
Use the [operations runbooks](../../deploy/runbooks.md) to investigate deployment failures.
