import { html, type Html } from "../html.ts";
import { HOMELAB_FEATURES, LICENSE_FREE_NODES, MIGRATIONS, MIGRATION_COMPARISON, docs } from "../content.ts";
import { external, icon } from "./layout.ts";

export function migrationCards(): Html {
  return html`<div class="migration-cards">
    ${MIGRATIONS.map((m) => html`<article>
      <p class="label">Coming from</p>
      <h3>${m.name}</h3>
      <p>${m.summary}</p>
      <p>${m.mapping}</p>
      <p class="migration-check"><strong>Before you switch.</strong> ${m.check}</p>
      ${external(docs(`/migrate/${m.slug}/`), html`Move from ${m.name} ${icon.arrow}`, "more")}
    </article>`)}
  </div>`;
}

export function moveOver(): Html {
  return html`<section class="sec migration-intro" aria-labelledby="homelab-title">
  <div class="wrap">
    <div class="homelab-hero">
      <div>
        <p class="label">DNS for your homelab</p>
        <h1 id="homelab-title" class="page-title">Your network.<br>Your DNS rules.</h1>
        <p class="sec-lead">Local names for your services, ad blocking for your household, and a dashboard that explains every answer. Run kudurru on the hardware you already have, in one container or your Kubernetes cluster.</p>
        <div class="actions">
          ${external(docs("/start/home/"), html`Set up your home network ${icon.arrow}`, "btn btn-primary")}
          <a class="btn btn-ghost" href="#homelab-migrate-title">Moving an existing setup? ${icon.arrow}</a>
        </div>
      </div>
      <aside class="homelab-key" aria-labelledby="homelab-key-title">
        <p class="label">Community / Homelab</p>
        <h2 id="homelab-key-title">Free for your homelab.</h2>
        <p>Eligible personal and household use is free under the Kudurru Community License. Start with one node, the default policy and the built-in administrator, without a key.</p>
        <ul class="points">
          <li>A free key covers up to ${LICENSE_FREE_NODES} nodes</li>
          <li>Add policies for different devices and subnets</li>
          <li>Unlock OIDC, long-term statistics and the dnsdist / keepalived edge</li>
        </ul>
        <a class="more" href="/license/">Request a free key ${icon.arrow}</a>
        <p class="fine">Running DNS for a business or government? <a href="/enterprise/">Explore commercial licensing ${icon.arrow}</a></p>
      </aside>
    </div>
    <div class="migration-cards homelab-features">
      ${HOMELAB_FEATURES.map((feature) => html`<article>
        <h3>${feature.title}</h3>
        <p>${feature.body}</p>
        ${external(docs(feature.href), html`${feature.link} ${icon.arrow}`, "more")}
      </article>`)}
    </div>
  </div>
</section>
<section class="sec migration-section" aria-labelledby="homelab-deploy-title">
  <div class="wrap">
    <div class="sec-head">
      <p class="sec-label">Make it yours</p>
      <h2 id="homelab-deploy-title">One container.<br>Or the cluster you built.</h2>
    </div>
    <div class="migration-cards homelab-deploy">
      <article>
        <p class="label">Docker &amp; Compose</p>
        <h3>Start with a single host</h3>
        <p>DNS, the dashboard and the API run in one container, with one persistent volume and no separate database. Give the host a stable address and keep DHCP on your router.</p>
        ${external(docs("/deploy/docker/"), html`Run with Docker ${icon.arrow}`, "more")}
      </article>
      <article>
        <p class="label">Kubernetes &amp; Helm</p>
        <h3>Keep your DNS in Git</h3>
        <p>Manage zones, records and filtering as Kubernetes resources. Deploy with Helm and sync configuration with your GitOps tool. Start with one replica; a free key covers up to ${LICENSE_FREE_NODES} nodes.</p>
        ${external(docs("/start/kubernetes/"), html`Deploy with Helm ${icon.arrow}`, "more")}
      </article>
    </div>
  </div>
</section>
<section class="sec migration-section" aria-labelledby="homelab-migrate-title">
  <div class="wrap">
    <div class="sec-head">
      <p class="sec-label">Moving your existing setup</p>
      <h2 id="homelab-migrate-title">Bring your DNS with you.</h2>
    </div>
    <p class="sec-lead">Already running Pi-hole, AdGuard Home or Technitium? Keep the lists and local names you rely on. Each guide maps your configuration and explains what changes before you switch your home network.</p>
    ${migrationCards()}
    <div class="actions"><a class="btn btn-ghost" href="#comparison-title">Compare all four DNS servers ${icon.arrow}</a></div>
  </div>
</section>
<section class="sec migration-section" aria-labelledby="comparison-title">
  <div class="wrap">
    <div class="sec-head">
      <p class="sec-label">Feature comparison</p>
      <h2 id="comparison-title">Know what changes.</h2>
    </div>
    <p class="sec-lead">kudurru v0.1.0 combines authoritative DNS, forwarding, filtering and a dashboard. Check the features you depend on before moving a working network.</p>
    <div class="shapes migration-table" role="region" aria-labelledby="comparison-title" tabindex="0">
      <table>
        <caption>Built-in capabilities unless stated otherwise. Comparison reviewed October 15, 2026; other projects evolve independently.</caption>
        <thead><tr><th scope="col">Feature</th><th scope="col">Pi-hole</th><th scope="col">AdGuard Home</th><th scope="col">Technitium</th><th scope="col">kudurru v0.1.0</th></tr></thead>
        <tbody>${MIGRATION_COMPARISON.map((r) => html`<tr><th scope="row">${r.feature}</th><td>${r.pihole}</td><td>${r.adguard}</td><td>${r.technitium}</td><td>${r.kudurru}</td></tr>`)}</tbody>
      </table>
    </div>
    <p class="fine migration-sources">Sources: ${MIGRATIONS.map((m, i) => html`${i ? " · " : ""}${external(m.source, m.name)}`)} · ${external(docs("/project/changelog/"), "kudurru changelog")}. ${external(docs("/migrate/"), "Detailed mappings and limitations in the docs")}.</p>
  </div>
</section>
<section class="sec migration-section" aria-labelledby="migration-path-title">
  <div class="wrap">
    <div class="sec-head">
      <p class="sec-label">The migration path</p>
      <h2 id="migration-path-title">Back up. Compare. Switch.</h2>
    </div>
    <ol class="migration-steps">
      <li><h3>Build alongside your current server</h3><p>Back up the source configuration. Give kudurru a separate address, copy lists and local names, and recreate client rules using your product's guide above. Product backup archives cannot be imported directly.</p></li>
      <li><h3>Test the behavior you rely on</h3><p>Check allowed and blocked names, local records, reverse lookups and each client group. Use real DNS queries as well as the dashboard's policy explanations.</p></li>
      <li><h3>Switch a client, then the network</h3><p>Try one device first. Update DHCP and IPv6 DNS advertisements after it passes. Keep the original server and backup ready until leases and caches have expired.</p></li>
    </ol>
    <div class="actions">${external(docs("/migrate/cutover/"), html`Follow the cutover and rollback checklist ${icon.arrow}`, "btn btn-primary")}</div>
  </div>
</section>`;
}
