# Connect a client

**Before you start**

- You need a running node with a fixed address.
- You need access to the DNS settings of one test client.
- You need `dig` or `nslookup` on that client.

## 1. Test the server directly

1. Open a terminal on the test client.
2. Query your node, with its address in place of `<host>`:

   ```sh
   dig @<host> example.com
   ```

   On Windows, use `nslookup example.com <host>`.

The response shows an address for `example.com`. If it times out, check
UDP and TCP port 53 between the client and the node.

## 2. Make the client use kudurru

1. Open the network settings of the test client.
2. Set its DNS server to the address of your node.
3. Save the settings.
4. Query without an explicit server:

   ```sh
   nslookup example.com
   ```

The **Server** address in the response matches your node.
Open **Query log** in kudurru. The query appears with the address of the client.

## 3. Connect the rest of the network

Choose one method after the test client works:

| Method | What to change |
|---|---|
| Keep DHCP on your router | Set the DNS server in the router's LAN DHCP settings to your node |
| Let kudurru assign addresses | Follow [DHCP setup](../dhcp/README.md) |

Renew the DHCP lease on one client after a router change. Other clients get
the new DNS setting when their leases renew.
Keep IPv6 DNS advertisements consistent with your DNS plan.
A public secondary DNS server can let clients bypass kudurru's rules.

## Check the result

Run `nslookup example.com` again with no server argument.
Confirm the server address, then find the query in **Query log**.
A query with `@<host>` tests the node directly; it does not check the client's default DNS setting.
