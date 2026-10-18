---
title: Home network in one container
linkTitle: Home network
audience: Home user
description: One `docker run` or compose file, the dashboard on port 8080, blocking with per-device rules, and local names for the things on your network.
weight: 10
---

One node serves DNS and the dashboard, with its data on a persistent volume.
Follow the [First steps walkthrough](/guide/first-steps/) for installation,
<a id="2-sign-in"></a>sign-in and your first client queries.

## Build your home network

| Task | Walkthrough |
|---|---|
| <a id="1-start-it"></a>Start a node and open the app | [First steps](/guide/first-steps/) |
| <a id="4-blocking"></a>Block ads and trackers | [Add a block list](/guide/filtering/add-list/) |
| Fix a blocked site | [Allow a blocked site](/guide/filtering/allow-site/) |
| <a id="3-point-the-network-at-it"></a>Let kudurru assign IPv4 addresses | [Set up DHCP](/guide/dhcp/) |
| Keep a device at a fixed address | [Reserve an address](/guide/dhcp/reservations/) |
| <a id="6-names-for-your-own-devices"></a>Name a device on the network | [Create a local DNS name](/guide/local-dns/create-name/) |

<a id="5-rules-per-device"></a>

For different rules per device, see
[Groups, clients and policies](/guide/admin/05-groups-clients-policies/).
A policy other than `default` needs a [license key](/project/licensing/).
<a id="7-resolve-names-yourself-optional"></a>

For recursive resolution, see
[Resolve names from the root servers](/guide/admin/07-zones-and-upstreams/#resolve-names-from-the-root-servers).

<a id="8-keep-it-running"></a>

For deployment options, volumes, secrets and TLS, use the
[Docker guide](/deploy/docker/).
