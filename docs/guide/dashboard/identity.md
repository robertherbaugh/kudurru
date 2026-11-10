# Check users, roles and sign-in providers

**Before you start**

- You need the `admin` role to inspect identity configuration.
- You need a license key for additional roles and single sign-on.
- You need access to the configuration file or Kubernetes resources to change identity settings.

## Inspect identity configuration

1. Open **System** > **Access**.
2. Select **Identity**.
3. Read **Local users** for configured local accounts.
4. Read **External accounts** for accounts from single sign-on.
5. Read **Service accounts** for program accounts and token counts.
6. Read **Role bindings** for subjects, roles and scopes.
7. Read **Identity providers** for configured sign-in providers.

![The Identity tab with the Identity management message, local users, external accounts, service accounts and role bindings.](../images/access-identity.png)

*Figure 1. The dashboard shows the identity configuration. You change role bindings and providers at their source.*

The tab has two kinds of action. In **Local users**, an administrator can
[remove the authenticator of a person](../admin/09-people-and-tokens.md#remove-the-authenticator-of-a-different-person).
In **External accounts**, an administrator can disable, enable or sign out an
account ([External accounts](access.md#external-accounts)). A node with
Kubernetes storage does not have these actions.

A role binding connects a user, group or service account to a role.
A zone scope limits a `zone-admin` binding to one zone.
A sign-in provider alone does not grant access; a matching role binding is also required.

## Change access at its source

Use the [role binding procedure](../admin/09-people-and-tokens.md#give-a-role-to-a-person)
for configuration examples and Kubernetes resources.
Use the [single sign-on reference](../admin/09-people-and-tokens.md#single-sign-on)
for provider fields. Changes to file-based `rbac.bindings` need a node restart.
Keep a working administrator session while you validate new access.

## Check the result

Sign in with the affected account in another browser session.
Confirm that the intended pages and actions are available.
Read its roles on **You & tokens** before you remove old access.
