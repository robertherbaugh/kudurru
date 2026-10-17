# Change and test a policy

**Before you start**

- You need the `operator` or `admin` role to edit a policy.
- You need the address of a client in one of its groups.

## Check the active rules

1. Open **Filtering** > **Policies**.
2. Open the policy by its name.
3. Read **Right now** for its state, priority and schedule.
4. Read **Shadowed by** if another policy takes priority.
5. Click **Edit** in the section you need to change.
6. Save the changes.

![A policy detail page with its current state, schedule, priority and editable sections.](../images/policy-detail.png)

*Figure 1. Scheduled off and Disabled policies do not apply now.*

**Shadowed** means another policy decides settings for the same clients.
kudurru still merges applicable lists. A schedule uses its configured time zone.
File-managed and Kubernetes-managed policies need edits at their source.

## Block a service

1. Find **Blocked services** on the policy page.
2. Find the service with **Search by name or domain**.
3. Click its tile to select it.
4. Click **Save services**.

**Block the whole category** also includes services added to that category later.
Selecting a tile in a blocked category creates an exception for that service.
An allow list still wins over a service block.

## Check the result

Open [Tools](tools.md). Query the service's name with the client's address in **As client**.
Check the result in [client details](clients.md) and [Query log](query-log.md).
If the client bypasses kudurru with another DNS service or a VPN, these rules cannot affect its queries.
