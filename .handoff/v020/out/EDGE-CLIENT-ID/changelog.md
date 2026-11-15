### Added

- The dnsdist edge passes client IDs to kudurru: with `edge.clientIDDomain` (`--client-id-domain`) a DoT or DoQ client connecting to `phone.dot.example.com`, and with `edge.clientIDs` (`--client-ids`) a DoH or DoH3 client using `/dns-query/phone`, selects the same client group and policy as it would directly against kudurru. The ID travels in the secret-authenticated PROXYv2 TLV 0xE1, so the edge requires `proxyProtocolSecretFile`. Queries with an ID skip the edge packet cache, and the edge answers HTTP 404 for the paths kudurru refuses.

### Known issues

- The pinned dnsdist 2.1.3 edge never answers a DoQ or DoH3 request whose QUIC end of stream (STREAM FIN) arrives in its own empty frame after the query data; the client waits until it times out and the query never reaches kudurru. This happens with or without client IDs. Clients that send the FIN with the query are answered, and kudurru's own DoQ and DoH3 listeners answer both forms.
