// Search landing pages: each answers a different deployment question, then
// sends the reader to the maintained setup or migration guide. Keep current
// capabilities and limitations aligned with site/content/start and migrate.
import { docs } from "./content.ts";

export interface UseCase {
  path: string;
  label: string;
  title: string;
  description: string;
  heading: string;
  intro: string;
  summary: string;
  action: { label: string; href: string };
  extraLink?: { label: string; href: string };
  sections: {
    title: string;
    paragraphs: string[];
    link: { label: string; href: string };
  }[];
  questions: { question: string; answer: string }[];
}

export const USE_CASES: readonly UseCase[] = [
  {
    path: "/kubernetes-dns/",
    label: "Kubernetes DNS",
    title: "Kubernetes DNS server with CRDs & Helm | kudurru",
    description: "Manage DNS zones, records and blocking as Kubernetes CRDs. Deploy kudurru with Helm and keep answering DNS queries through control plane outages.",
    heading: "Kubernetes DNS you manage with kubectl.",
    intro: "Run authoritative DNS, a forwarding or validating recursive resolver and network ad blocking in your cluster. kudurru stores configuration in Kubernetes custom resources, so zones, records and policies fit the same Git review and deployment workflow as your applications.",
    summary: "Manage zones and policies as CRDs, deploy with Helm, and serve DNS from memory on every replica.",
    action: { label: "Deploy with Helm", href: docs("/start/kubernetes/") },
    sections: [
      {
        title: "DNS configuration as Kubernetes objects",
        paragraphs: [
          "DNSZone and DNSRecordSet define authoritative zones and records. BlockList, ClientGroup and Policy define filtering and which clients it applies to. Apply the objects with kubectl or sync them with your GitOps tool; the dashboard shows which source owns each object.",
          "The external-dns webhook can write DNSRecordSet objects from your Kubernetes resources. That integration requires a license key. Records remain visible as typed objects instead of disappearing into a separate configuration database.",
        ],
        link: { label: "Explore the DNS custom resources", href: docs("/reference/crds/") },
      },
      {
        title: "Answers through control plane outages",
        paragraphs: [
          "Every replica watches the resources and answers from an in-memory snapshot. The query path does not read the Kubernetes API. A running node keeps answering from its last good state when the apiserver is unavailable; configuration changes wait for it to recover.",
          "One elected controller handles work such as zone serial updates, list fetching and NOTIFY. The Helm charts support persistent local snapshots, topology spread and monitoring. Start with one replica without a key; multiple nodes require a key, with free personal keys available for eligible use.",
        ],
        link: { label: "Read the Helm deployment guide", href: docs("/deploy/helm/") },
      },
      {
        title: "Choose how clients reach DNS",
        paragraphs: [
          "Expose DNS using a LoadBalancer Service, MetalLB or kube-vip, or use host networking. Preserving client IP addresses matters when you apply different policies to different devices or networks.",
          "kudurru serves the zones and networks you configure. Installing it does not automatically replace your cluster's service discovery DNS. Keep that role with your existing cluster DNS unless you deliberately design and test a forwarding arrangement.",
        ],
        link: { label: "Plan listeners and network access", href: docs("/start/network/") },
      },
    ],
    questions: [
      { question: "Can I run kudurru on a single-node k3s cluster?", answer: "Yes. Use one replica with Kubernetes CRDs, or the chart's standalone mode with SQLite on a persistent volume. The Helm guide describes both shapes." },
      { question: "Does kudurru require a separate database in Kubernetes?", answer: "No. In Kubernetes mode, the apiserver stores configuration as custom resources. DNS answers come from memory, with local state used for snapshots and query data." },
    ],
  },
  {
    path: "/network-ad-blocking/",
    label: "Network ad blocking",
    title: "Network ad blocking & self-hosted DNS filtering | kudurru",
    description: "Block ad and tracker domains across your network with self-hosted DNS. Run kudurru in Docker or Kubernetes with blocklists, client policies and a query log.",
    heading: "DNS ad blocking for the network you run.",
    intro: "Make kudurru the DNS server your router hands out to devices. Filter ad and tracker domains for clients using that resolver, keep local DNS names alongside your blocklists, and see the rule behind each blocked answer in one dashboard.",
    summary: "Filter ad and tracker domains across your network, with a query log that explains each answer.",
    action: { label: "Set up your home network", href: docs("/start/home/") },
    sections: [
      {
        title: "One container, your own resolver",
        paragraphs: [
          "Run kudurru in Docker with a persistent data volume. The container includes DNS, the dashboard and the API; you do not need a separate database. A starter blocklist is embedded, and you can add URL sources or your own entries in hosts, domain, adblock and regex formats.",
          "Give the host a stable address and configure your DHCP server to advertise it as the DNS server. v0.1.0 includes opt-in DHCPv4 on Linux/SQLite, with dashboard setup and optional three-node quorum failover. You can also keep DHCP on your router. Test with one client before switching the whole network.",
        ],
        link: { label: "Start one Docker container", href: docs("/deploy/docker/") },
      },
      {
        title: "Find out why a domain was blocked",
        paragraphs: [
          "The query log names the matching list and rule, the client, the upstream and the node that answered. Allow or deny a domain directly from the log, disable an individual list entry, or pause blocking for a set amount of time while you troubleshoot.",
          "Client groups match IP addresses and CIDR ranges. Policies can give a subnet its own lists and schedule, such as a kids' VLAN. Policies beyond the default require a license key; eligible personal users can request a free key.",
        ],
        link: { label: "Learn about groups and policies", href: docs("/guide/admin/05-groups-clients-policies/") },
      },
      {
        title: "Understand the limits of DNS filtering",
        paragraphs: [
          "DNS filtering blocks domain names, not individual URLs or page elements. It cannot selectively remove an ad served from the same hostname as the content you want. A browser content blocker can complement network DNS filtering.",
          "Devices that use another resolver, a VPN's DNS or their own encrypted DNS settings can bypass your network resolver. kudurru supports DoT, DoH, DoQ and DoH3 for clients you configure to use it; supporting those transports does not force other clients to use them.",
        ],
        link: { label: "Configure your network and encrypted DNS", href: docs("/start/network/") },
      },
    ],
    questions: [
      { question: "Do I need to install an app on every device?", answer: "No app is needed for clients that use the DNS server advertised by your router. Devices with their own DNS or VPN settings may need additional configuration." },
      { question: "Can kudurru resolve names without Cloudflare or Quad9?", answer: "Yes. Set an upstream group to recursive mode and kudurru asks the root servers and each domain's own name servers, with QNAME minimization and DNSSEC validation, so no single provider sees every name. Those servers still see the names they serve and your public address, and the queries travel as plain DNS on port 53, which your network must allow. Forwarding stays the default." },
      { question: "Is kudurru free for a home network?", answer: "Eligible personal use is free under the Kudurru Community License. One node with the default policy works without a key. A free personal key unlocks additional policies and licensed features for up to three nodes. Business and government production use requires a commercial agreement." },
    ],
  },
  {
    path: "/pi-hole-alternative/",
    label: "Pi-hole alternative",
    title: "A Pi-hole alternative for Docker & Kubernetes | kudurru",
    description: "Looking beyond Pi-hole? Explore kudurru for DNS blocking, authoritative zones, encrypted DNS and Kubernetes CRDs, with a practical guide to manual migration.",
    heading: "Looking for a Pi-hole alternative?",
    intro: "kudurru combines network DNS blocking with authoritative zones, encrypted DNS transports and a dashboard for multiple nodes. It may fit if you want to keep familiar lists and client groups while managing DNS in Docker or Kubernetes.",
    summary: "See what carries over from Pi-hole, what changes, and how to move clients gradually.",
    action: { label: "Read the Pi-hole migration guide", href: docs("/migrate/pihole/") },
    extraLink: { label: "Coming from AdGuard Home", href: docs("/migrate/adguard-home/") },
    sections: [
      {
        title: "Familiar blocking, typed configuration",
        paragraphs: [
          "Use URL blocklists, exact domains, wildcard and regex rules, client groups, and one-click allow and deny from the query log. Each answer records why it was given, so you can trace an unexpected block to its list and rule.",
          "Configuration is stored as BlockList, ClientGroup and Policy objects. The same object YAML works with Docker and Kubernetes. Local names become records in an authoritative DNSZone; conditional forwarding sends selected domains to another resolver.",
        ],
        link: { label: "Map Pi-hole concepts to kudurru", href: docs("/migrate/pihole/") },
      },
      {
        title: "When kudurru is a useful fit",
        paragraphs: [
          "Consider kudurru when you want DNS zones and network filtering in one server, encrypted DNS listeners for DoT, DoH, DoQ and DoH3, or Kubernetes custom resources managed through Helm and GitOps. The dashboard can show queries and health across multiple nodes.",
          "kudurru uses the Kudurru Community License. Eligible personal and nonprofit use is free; business and government production needs a commercial agreement. Additional policies, multiple nodes and features such as OIDC require a key. Check the license guide against the setup you plan to run.",
        ],
        link: { label: "Check licensing and key requirements", href: "/license/" },
      },
      {
        title: "Plan a gradual migration",
        paragraphs: [
          "Use the migration guide to transfer your lists, groups and local records. kudurru v0.1.0 includes opt-in DHCPv4 on Linux/SQLite, with dashboard setup, durable leases, reservations and dnsmasq lease import. You can keep DHCP on your router or prepare kudurru before disabling Pi-hole's DHCP service. DHCPv6 is not supported.",
          "Run kudurru on a separate address beside Pi-hole. Compare answers for representative clients, move a test device, then change the DNS address advertised by DHCP. Keep Pi-hole available until its query log is quiet. If you ran Unbound beside Pi-hole, a recursive upstream group can replace it, with DNSSEC validation. Pi-hole API compatibility is planned for v1.0.",
        ],
        link: { label: "Follow the migration checklist", href: docs("/migrate/pihole/#switching-over") },
      },
    ],
    questions: [
      { question: "Can I import my Pi-hole backup directly?", answer: "No. Migration is currently manual. The guide maps Pi-hole groups, adlists, domain rules, local records and upstreams to kudurru objects. Keep your existing Pi-hole installation while you test the new configuration." },
      { question: "Can I also move from AdGuard Home?", answer: "There is a separate AdGuard Home migration guide in the documentation. It explains how to recreate filtering, client policies, DNS rewrites and upstream settings; migration is manual there too." },
    ],
  },
];
