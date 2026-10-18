# Compare DNS activity over time

**Before you start**

- You need the `viewer` role or a higher role.
- You need a license key for statistics older than 24 hours.
- The nodes need collected statistics for the period you select.

## Choose a period

1. Open **Overview** > **Long-term**.
2. Select the node scope.
3. Select **7 days**, **30 days**, **90 days** or **1 year**.
4. Read the query count, blocked count, blocked share and cache count.
5. Read the time chart for peaks or gaps.

![The Long-term page with a selected period, totals, a time chart and top domain tables.](../images/long-term.png)

*Figure 1. The chart groups queries into time buckets.*

Use **From** and **To** for a custom period.
**Data table** shows the chart values as text.
The top domain and client tables show approximate counts.
Use [Query log](query-log.md) when you need individual retained queries.

## Check the result

Switch to another period and confirm that the range and totals update.
If the page asks for a key, [check License](license.md).
An empty range can mean the nodes have no statistics for that period.
