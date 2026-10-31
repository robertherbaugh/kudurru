# Find and explain a DNS answer

**Before you start**

- You need the `auditor`, `operator` or `admin` role.
- You need the client address and the approximate time of its query.

## Find the query

1. Open **Overview** > **Query log**.
2. Select the node scope.
3. Enter the device address in **Client**.
4. Enter the name in **Domain**.
5. Press **Enter**.
6. Click a query row to expand its details.

![A query row expanded with its matched list, rule, client and response details.](../images/query-detail.png)

*Figure 1. The matched rule explains a blocked answer.*

Use **Status** to select **Blocked** or **Errors**. Use **Since** and **Until**
to narrow the time. `*.example.com` matches subdomains in **Domain**.
Click **Clear filters** to start again. The page URL preserves the filters.

## Download matching history

Select **Export up to 1,000 queries** to download the newest matching retained
queries as JSON, using the current filters and node scope. The file includes
pagination and scope information; `partial: true` identifies unavailable nodes,
and `cursor_next` means more matching rows are available through the API.
Exports preserve the privacy applied when the queries were recorded. Live-tail
rows still waiting for disk persistence are not included. Narrow the filters
or time range when you need a smaller download.

The API endpoint is `GET /api/v1/queries/export`, with the same filters as
`GET /api/v1/queries`. It accepts `limit` up to 1,000 and the returned cursor
with the same filters and scope for another page. Each request is authorized
independently; separate downloads are not a cross-page database snapshot.
The output limit is 16 MiB and the read deadline is 15 seconds. A failed or
canceled read returns an error before attachment headers are sent.

## Watch a retry

1. Turn on **Live tail**.
2. Retry the query from the client.
3. Read the new row's status, matched list, upstream and DNSSEC result.
4. Turn off **Live tail** to return to history.

An `operator` or `admin` can use **Allow** or **Deny** on a row.
These actions create exact entries for all clients. Use the
[Allowlist guide](../filtering/allow-site.md) for a narrower client scope.

If a node disconnects, results can be partial. Reload history after it recovers.
Privacy settings can hide names or client addresses.

CAUTION: **Purge…** deletes query history. Use the
[purge procedure](../admin/03-watch-the-network.md#delete-old-rows) only when you intend to delete it.

## Check the result

Find the test client's retry with its expected status.
If its rules look wrong, [check the client's group and policy](clients.md).
