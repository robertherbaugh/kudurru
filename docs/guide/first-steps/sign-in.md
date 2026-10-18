# Sign in to the dashboard

**Before you start**

- You need a running node and its address.
- You need the password in `secrets/admin_password` from the previous step.

## 1. Open the dashboard

1. Open `https://<host>:8080/` in your browser.
2. Check that the address belongs to your node before you accept its certificate.
3. Enter `admin` in **Username**.
4. Enter the password in **Password**.
5. Click **Sign in**.

![The sign-in page with Username and Password fields and the Sign in button.](../images/signin.png)

*Figure 1. Sign in with the local account.*

A new node creates a self-signed certificate. Use your own certificate to
remove the browser warning. See [TLS setup](../admin/02-install-and-sign-in.md#sign-in).
Your page has single sign-on buttons only if an administrator configured them.

## 2. Change the first password

1. Open **Change password** with the key icon at the top right.
2. Enter the **Current password**.
3. Enter a **New password** with at least 8 characters.
4. Enter it again in **Confirm password**.
5. Click **Change password**.

![The Change password dialog with the current password and the two new password fields.](../images/change-password.png)

*Figure 2. Set a password for your account.*

## Check the result

The dashboard opens. A new node shows zero queries until a client uses it.
The message **Default credentials are active** goes away after the password change.

If you lost the password, use the
[password recovery steps](../admin/12-troubleshooting.md).
