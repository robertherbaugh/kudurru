# Choose upstream DNS servers

**Before you start**

- You need the `admin` role to change upstream groups.
- You need reachable resolvers and working bootstrap DNS for resolver hostnames.

## Create a group

1. Open **DNS** > **Upstreams**.
2. Click **New group**.
3. Select a provider on **Provider**, or select **Custom**.
4. Click **Next** to open **Servers**.
5. Select the filtering option and **Transport**.
6. Click **Next** to open **Group**.
7. Enter a **Name** and select a **Strategy**.
8. Click **Next** to open **Review**.
9. Click **Create group**.

![The Servers step for Quad9 with transport and server choices.](../images/upstream-wizard-servers.png)

*Figure 1. An encrypted transport protects queries on the path to the upstream.*

For **Custom**, use addresses such as `9.9.9.9:53`, `tls://dns.quad9.net`
or `https://dns.quad9.net/dns-query`.
Add a plain `address:port` in **Bootstrap resolvers** when resolver hostnames need it.

## Use and maintain the group

1. Open the group's detail page.
2. Check each server's health, RTT and errors.
3. Select the group in a policy or a forward zone.
4. Query a name that uses that policy or zone.

Creating a group alone does not change where clients' queries go.
The `default` group handles queries without another route.
To route one suffix, add it under **Conditional forwarding**, select its group,
then click **Save rules**. The longest suffix wins.

Edit **Settings** on the group detail page to change servers, strategy or timeout.
You cannot delete `default` or a group that is still in use.
For recursion and DNSSEC, follow the [resolver procedure](../admin/07-zones-and-upstreams.md#resolve-names-from-the-root-servers).

## Check the result

The group shows healthy servers. [Query log](query-log.md) names the expected upstream for a test query.
Use [Tools](tools.md) with **As client** when policies choose different groups.
