# Manage a list and its sources

**Before you start**

- You need the `operator` or `admin` role to change a list.
- You need an existing list. Use [Add a block list](../filtering/add-list.md) to create one.

## Check a list

1. Open **Filtering** > **Lists**.
2. Click **Open** for the list.
3. Read **Entries**, **Invalid lines** and **Unsupported rules**.
4. Read **Used by** to find the policies that use it.
5. Click **Refresh now** to fetch URL sources again.

![A list detail page with source status, entry counts, settings and policies that use the list.](../images/list-detail.png)

*Figure 1. A list needs a policy before it affects clients.*

Use **Add source** for another URL, ConfigMap, file or pasted entries.
Use a source's **Edit** action to change it.
The **Enabled** switches control a whole list or an individual source.
An unsupported rule is parsed but does not affect queries.

## Turn off one source entry

1. Click **Entries** in the source row.
2. Enter the domain in **Search entries**.
3. Turn off **Enabled** for that entry.

This affects only this list. Another list can still block the same name.
Use the [Allowlist](../filtering/allow-site.md) for an exception across block lists.

If the owner is a file or Kubernetes, edit the source configuration.
The dashboard cannot change an object that another owner manages.

## Check the result

Run a query as an affected client with [Tools](tools.md).
Confirm the matched rule in [Query log](query-log.md).
If the list has fetch or parsing errors, open [Diagnosis](diagnosis.md).
