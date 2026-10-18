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

## Check the result

All expected nodes are ready and use the intended version.
If a node is unavailable, treat combined statistics as partial until it recovers.
Use the [operations runbooks](../../deploy/runbooks.md) to investigate deployment failures.
