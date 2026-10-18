# Add a block list

**Before you start**

- You need the `admin` or `operator` role.
- You need a policy that you can edit in the dashboard.

A new node already uses the small `starter` list. Add another list when
you need more coverage.

## 1. Choose a source

1. Open **Filtering** > **Lists**.
2. Click **New list**.
3. Select **Block list** on the **Type** step.
4. Click **Next**.
5. Select one of the **Well-known lists** on the **Sources** step.

![The New list wizard on Sources, with well-known block lists and their licenses.](../images/list-wizard-sources.png)

*Figure 1. Choose a source for the list.*

## 2. Attach the list to a policy

1. Click **Next** to open **Options**.
2. Enter `home-ads` in **Name**.
3. Select `default` in **Use in policies**.
4. Keep **Start enabled** on.
5. Click **Next**.
6. Check the summary on **Review**.
7. Click **Create list**.

![The Options step with Name, Use in policies and the Start enabled switch.](../images/list-wizard-options.png)

*Figure 2. A policy must use the list for it to affect clients.*

If **Review** says **No policy uses this list**, go back and select a policy.
For a policy managed by a file or Kubernetes, change its `blockListRefs` at the source.

## Check the result

Open the new list. Check that **Entries** is greater than zero and **Health** shows `OK`.
Search for a name from that list in **Which lists contain a domain?**.
Query that name from a client, then check its result in **Query log**.

If a site stops working, follow [Allow a blocked site](allow-site.md).
