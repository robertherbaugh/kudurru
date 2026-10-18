# Review and answer an access request

**Before you start**

- You need the `operator` or `admin` role to answer requests.
- You need a license key for access requests.
- Users need a configured [block page](../admin/06-access-requests.md#turn-on-the-block-page) with the request form enabled.

## Review the request

1. Open **Filtering** > **Requests**.
2. Find a request with the state **Pending**.
3. Click its name to open the detail page.
4. Read the domain, requester, reason and decision history.
5. Read recent queries if your role can access them.

![The Requests page with pending requests, client details and decision actions.](../images/requests.png)

*Figure 1. Review the name and client before you approve access.*

## Approve focused access

1. Click **Approve**.
2. Select **Everyone** or **The requester's group** in **Scope**.
3. Select **This name only** in **Match**.
4. Select a **Duration**.
5. Enter a **Note** that explains the decision.
6. Click **Approve**.

An approval adds an allow entry. A time limit ends automatically.
A group approval needs a policy that covers that group.
Managed policies need their allow list references updated at the source.

To refuse a pending request, click **Deny**, enter a note, then click **Deny**.
To revoke an approval, find the approved request and deny it.

CAUTION: Deleting a request with permanent approval does not remove its allow entry.
Deny the approval first when you intend to revoke access.

## Check the result

The request detail page records the decision and its note.
Ask the requester to retry the site. Check the client's query log for the new answer.
Use [Allowlist](../filtering/allow-site.md) to inspect the resulting rule.
