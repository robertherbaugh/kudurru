# Local DNS

Give a device a name that clients can use on your network.

| Task | Guide |
|---|---|
| Create a zone and a permanent device name | [Create a local DNS name](create-name.md) |
| Create names from active DHCP leases | [Create a DHCP scope](../dhcp/create-scope.md) |
| Change external resolvers or use recursive resolution | [Zones and upstreams](../admin/07-zones-and-upstreams.md#upstreams) |

A primary zone stores your own records. An upstream answers names outside
your zones. A forward zone sends one domain to a specific upstream group.
