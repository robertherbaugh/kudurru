---
title: Migration guides
linkTitle: Migrate
description: How the concepts of the DNS server you run today map onto kudurru objects and settings.
weight: 30
---

kudurru has no importer for Pi-hole or AdGuard Home configuration yet: the
`kudurru import pihole|adguard|blocky|technitium` verbs are planned
([follow-ups](../../../docs/followups.md)), and today `kudurru import bind` is the only
importer. Moving over is a short manual job, because the concepts map almost
one to one: write the objects once as YAML, keep them in Git, and apply them
with `kudurru apply -f` (or drop them into `objects.d/`).

Run the new server next to the old one, compare answers with
`kudurru query <name> --client <address>`, and switch the DHCP DNS option
when they agree.
