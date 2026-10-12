# 12. Troubleshooting

This chapter lists problems, their causes and their solutions. Start with
the first section. It shows the tools that find most causes.

## Find the cause

Use these tools in this order:

1. Open **System** > **Diagnosis**. A message often names the cause and the
   object.
2. Open **System** > **Tools**. Click **Run checks** in **Doctor**.
3. Open **Overview** > **Query log**. Find the query. Click the row to see
   the reason.
4. Use **Tools** > **Query (dig)** to repeat the query. Enter the address of
   the client in **As client**.
5. On the host, run `kudurru doctor` and read the log with `docker logs
   kudurru`.

## Start and sign-in

| Problem | Cause | Solution |
|---|---|---|
| The container does not start. The log says that the address is in use. | Another program uses port 53. On many Linux hosts it is `systemd-resolved`. | Set `DNSStubListener=no` in `/etc/systemd/resolved.conf`. Restart `systemd-resolved`. Start the container again. |
| The browser shows a certificate warning. | kudurru uses a self-signed certificate. | Accept the warning for a test. For production, set `listeners.api.tls.certFile` and `listeners.api.tls.keyFile`. |
| The sign-in is refused and the message mentions a rate limit. | Too many failed sign-ins. | Wait 5 minutes (one address) or 15 minutes (one user name). Then try again. |
| You lost the admin password. | - | See [Recover a lost admin password](11-maintenance.md#recover-a-lost-admin-password). |
| The dashboard signs you out. | The session had no activity for 30 minutes, or it lasted 12 hours. | Sign in again. An administrator can change the times. |
| The sign-in page has no button for single sign-on. | The provider has an error. | Open **Diagnosis**. A message names the provider. Check `auth.identityProviders`. |
| A person signs in with single sign-on and gets no access. | No role binding matches the person. | Add a role binding (Chapter 9). |
| A page shows **Not permitted**. | Your role does not have the permission that the message names. | Ask an administrator for a role with the permission. |

## Filtering

| Problem | Cause | Solution |
|---|---|---|
| A name is not blocked. | See [A name is not blocked](#a-name-is-not-blocked). | |
| A name is blocked and must not be. | See [A name is blocked by mistake](#a-name-is-blocked-by-mistake). | |
| A list shows the health `unavailable, cached`. | The last fetch failed. kudurru uses the copy from before. | Check that the node can reach the URL. Click **Refresh now** on the page of the list. Read the message in **Diagnosis**. |
| A list shows the health `invalid`, or **Invalid lines**. | The list has lines that are not valid in its format. | Check the **Format** of the source. Open the entries of the source. Correct or remove the lines. |
| A list has no effect. | No policy uses the list, or the list is off. | Open the page of the list. Check **Used by** and the switch **Enabled**. |
| The page **Domains** shows **Not applied for** and a policy. | The policy is managed by a file or by Kubernetes. kudurru cannot add the list to it. | Add the list `ui-allow` or `ui-deny` to the policy in the file or the resource. |
| A switch or button is gray. | The object is managed by a file or by Kubernetes, or your role cannot change it. | Look at the owner badge. Change the file or the resource. |
| A client gets the wrong rules. | The client is in a different group, or another policy has a higher priority. | Open the page of the client. Read **Groups** and **Policy right now**. |
| A schedule is not right. | The time zone of the policy is wrong. | Check the **Time zone** on the step **When**. |
| A change does not work at once. | The client or a resolver keeps the old answer for the block TTL. | Wait for the block TTL (10 minutes by default). Flush the cache of the client. |

### A name is not blocked

1. Open **Overview** > **Query log**. Enter the name in **Domain**.
2. Click the row of the query. Read **Status** and **Matched list**.
3. If there is no row, the client did not ask kudurru. The client can use
   another DNS server, a VPN, or the secure DNS setting of its browser.
   Make the client use kudurru, or block the encrypted resolvers with a
   block list.
4. If the status is `cached` or `forwarded`, no list matched the name.
   Open **Filtering** > **Lists**. Enter the name in **Which lists contain a
   domain?**. If a list contains it, check the next points.
5. Check that the list is on and that its health is `OK`. Check that a
   policy of the client group uses it.
6. Check that an allow entry does not allow the name. An allow entry wins.
7. Check that the name is not in one of your own zones. A block list cannot
   block these names.
8. Check that blocking is on. The status area must show **Blocking
   enabled**.
9. Remove the answer from the cache: **Tools** > **Response cache**.

### A name is blocked by mistake

1. Find the query in the **Query log** and open the row.
2. Read **Matched list** and **Rule**.
3. To allow the name for all clients, click **Allow** in the row.
4. To keep the list but turn off the one entry, open the list. Open the
   entries of the source and turn off the entry (Chapter 4).
5. To allow the name for one client group only, add it to the **Allowlist**.
   Set **Applies to** to **Some client groups**.

## DNS

| Problem | Cause | Solution |
|---|---|---|
| An upstream group shows `0 of N healthy`. | The servers do not answer. | Check the network from the node. Check the transport and the port. For a host name, for example `dns.quad9.net`, check the bootstrap resolvers. |
| Queries are slow. | The upstream is slow, or the timeout is long. | Open **Upstreams**. Read **RTT** on the page of the group. Select the strategy **Fastest first**. |
| A name in your zone does not resolve. | The record or the zone has a problem. | Open the zone. Check that **Ready** shows **Ready**. Check the record. Use **Query (dig)**. |
| A forward zone does not resolve. | The upstream group of the zone does not answer. | Check the upstream group. |
| A zone or record cannot be changed. | The owner is a file or Kubernetes. | Change the file or the resource. |
| Answers are old after a record change. | A cache keeps the old answer for the TTL. | Wait for the TTL. Use **Tools** > **Response cache** for the cache of kudurru. |

## Block page and requests

| Problem | Cause | Solution |
|---|---|---|
| The browser shows an error and not the block page. | The block page is off, has no address, or the port is not published. | Check the settings of Chapter 6. In Docker, check that the port is published. |
| The test shows `0.0.0.0` and not the block page address. | The block response is not **Block page**. | Set `blocking.defaultResponse` to `blockPage`, or set the block response of the policy. |
| A site that uses HTTPS shows a certificate warning. | The block page cannot have a valid certificate for the site. | This is normal. See Chapter 6. |
| The page shows no reason. | `blockPage.showDetails` is false, or the query log privacy hides the name or the client. | Change the setting. |
| The block page has no form. | `unblockRequests.enabled` is false. | Turn it on. |
| The page says that requests are paused. | The number of pending requests reached `unblockRequests.maxPending`. | Answer or delete requests. |
| A user does not see a request in the list. | The user asked on a different node, or the privacy mode hides the data. | Check **Requests** with the filter **State** set to **All**. |
| An approval does not work. | The group of the requester has no policy that kudurru can change. | Read the answer of the approval. Add the allow list to the policy in the file or the resource. |

## The system

| Problem | Cause | Solution |
|---|---|---|
| A setting changed back after a restart. | The node has no settings file. | Set `KUDURRU_SETTINGS_FILE`. Look for the banner **Kept in memory only** on the page **Settings**. |
| A setting does not take effect. | The setting needs a restart. | Look for the label `restart`. Restart the node. |
| A setting cannot be changed in the dashboard. | The setting is `host only`, or an environment variable sets it. | Change it in `kudurru.yaml` or in the environment of the host. |
| A statistics page says that the result is partial. | A node did not answer. | Open **Nodes** and find the node. Check its health. |
| The node shows **readiness unknown**. | The node is not reachable from the node that you use. | Check the network between the nodes (port 8082). |
| A node starts with an error about the data. | A newer version wrote the data. | Use the newer version. You cannot go back to an older one. |
| The dashboard is slow with a large query log. | The log has many rows. | Use filters. Delete old rows with **Purge…**. |

## Get more help

Collect this information before you ask:

- the version: `kudurru version`
- the result of `kudurru doctor`
- the messages on the page **Diagnosis**
- the relevant part of the log

Report a problem on the
[project page](https://github.com/robertherbaugh/kudurru/issues). Do not put
passwords, tokens or the encryption key in a report. To report a security
problem, follow [SECURITY.md](../../../SECURITY.md).
