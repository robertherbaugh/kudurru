# Create a client policy

**Before you start**

- You need the `operator` or `admin` role.
- You need a [client group](groups.md) and any [lists](../filtering/add-list.md) the policy uses.
- You need a license key for policies beyond `default`.

Without a key, [edit the default policy](policy-details.md) instead.
An allow list wins over block lists and blocked services.

## Start the policy

1. Open **Filtering** > **Policies**.
2. Click **New policy**.
3. Enter a **Name** on **Who**.
4. Select at least 1 entry in **Client groups**.
5. Enter a **Priority**.
6. Click **Next** to open **What**.

![The Who step with a policy name, client groups and priority.](../images/policy-wizard-who.png)

*Figure 1. A policy needs a client group before it can affect devices.*

Higher priority numbers decide settings when active policies overlap.
Lists from matching policies still merge.

## Choose the behavior

1. Select the **Block lists** and **Allow lists** on **What**.
2. Select any services or content categories to block.
3. Click **Next** to open **Safe search**.
4. Turn on **Enforce safe search** if needed.
5. Click **Next** to open **How**.
6. Select the block response, TTL and upstream group for this policy.
7. Click **Next** to open **When**.
8. Keep **Always**, or select **Only during a time window**.
9. Set the days, start, end and time zone for a time window.
10. Click **Next** to open **Review**.
11. Read the summary.
12. Click **Create policy**.

CAUTION: **Allow only** blocks every name except names on allow lists.
Keep it off unless you want that restriction.

Turn off **Log these clients' queries** only when you intend to exclude their
queries from the log and statistics.
Large content categories can need substantial memory on every node.

## Check the result

The policy detail page opens. Read **Right now** for its state.
Use [Check a policy](policy-details.md) to test an affected client before wider use.
