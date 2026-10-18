# Test DNS and run health checks

**Before you start**

- You need the `operator` or `admin` role for these operations.
- You need a DNS name and, for client rules, the client's address.

## Query as a client

1. Open **System** > **Tools**.
2. Enter a DNS name in **Name** under **Query (dig)**.
3. Select **Type** and **Transport**.
4. Enter the client's address in **As client**.
5. Click **Query**.
6. Read the status, answer, node and upstream.

![Tools with a completed Query (dig) result and its answer records.](../images/tools-query.png)

*Figure 1. As client tests the groups and policies for that address.*

The tool uses the DNS pipeline without sending through the network listener.
A successful result does not prove that clients can reach port 53.
Select **DNSSEC (DO bit)** when you need DNSSEC data in the answer.

## Remove a cached answer

1. Enter the affected name under **Response cache**.
2. Click **Flush name**.
3. Repeat the query from the real client.

An empty name changes the action to **Flush all**.
The client's own DNS cache can also retain an old answer.

## Run checks

1. Click **Run checks** under **Doctor**.
2. Read each `ok`, `warn` or `FAIL` result.
3. Correct the reported problem.
4. Click **Run checks** again.

These checks cover what the API can inspect. `kudurru doctor` on the host
also checks ports, file permissions and the data directory.
For configuration migration, use [Import configuration](import.md).
The export links download objects; use [host backups](../admin/11-maintenance.md#back-up) for full recovery.

## Check the result

The query gives the expected answer and relevant health checks pass.
Run `nslookup example.com` from the client to verify the actual network path.
