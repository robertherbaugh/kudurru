# Allow a blocked site

**Before you start**

- You need the `admin` or `operator` role.
- You need the address of a client that cannot open the site.

## 1. Find the blocked name

1. Open **Query log**.
2. Enter the client address in **Client**.
3. Select **Blocked** in **Status**.
4. Open the site again on the client.
5. Click a new query row to read its matched list and rule.

![A blocked query expanded to show the matched list and the rule that refused the name.](../images/query-detail.png)

*Figure 1. Find the DNS name that the site needs.*

A site can use several names. The blocked name can differ from the name
in the browser's address bar.

## 2. Add a focused exception

1. Open **Filtering** > **Allowlist**.
2. Click **Add to the Allowlist**.
3. Select the exact domain type in **Type**.
4. Enter the blocked name in **Domains**.
5. Select **Everyone** or **Some client groups** in **Applies to**.
6. Enter the reason in **Comment**.
7. Click **Add**.

![The Add domains dialog with the domain type, names, client scope and comment.](../images/domains-add.png)

*Figure 2. Allow the name for the clients that need it.*

The example image shows the combined **Domains** page. On **Allowlist**,
the action is already **Allow**.
An exact entry affects one name. Use a wildcard only when you need the whole domain.

## Check the result

Query the name again from the client. Confirm a permitted result in **Query log**.
If a cached block remains, open **System** > **Tools** and flush that name from the cache.
Then clear the client's DNS cache and retry the site.

If the page says **Not applied for**, update the named policy at its source.
See [Lists and domains](../admin/04-lists-and-domains.md#where-the-entries-are-stored).
