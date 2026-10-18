# Create a local DNS name

**Before you start**

- You need the `admin` role or permission to create and edit the zone.
- You need a fixed address for the device, such as a DHCP reservation.
- You need a client that uses kudurru for DNS.

This example gives a device the name `nas.home.arpa`.
Use a separate domain from your automatic DHCP lease names.

## 1. Create the zone

1. Open **DNS** > **Zones**.
2. Click **New zone**.
3. Select **Primary** on **Type**.
4. Click **Next**.
5. Enter `home.arpa` in **Zone name** on **Apex**.
6. Click **Next**.
7. Keep the defaults on **Records and access** for this example.
8. Click **Next**.
9. Click **Create zone** on **Review**.

![The New zone wizard with Primary selected on the Type step.](../images/zone-wizard-type.png)

*Figure 1. A primary zone holds the records you manage.*

If a zone already covers this domain, open that zone instead.
Do not create a zone over your DHCP domain unless you intend it to take precedence.

## 2. Add the device record

1. Open the new zone.
2. Click **Add record**.
3. Enter `nas` in **Name**.
4. Select **A (IPv4 address)** in **Type**.
5. Keep **TTL** at **Zone default**.
6. Enter the device's IPv4 address under **Target 1**.
7. Click **Add record**.

![The Add a record dialog with Name, Type, TTL and the IPv4 target fields.](../images/record-editor.png)

*Figure 2. Enter a name relative to the zone and the device address.*

The screenshot shows demo values. Use `nas` and your own device address for this example.

## Check the result

Run `nslookup nas.home.arpa` from the client. The answer contains the address
you entered. The **Query log** details identify your zone as the source.

For other record types, access rules and imports, see
[Zones and upstreams](../admin/07-zones-and-upstreams.md).
