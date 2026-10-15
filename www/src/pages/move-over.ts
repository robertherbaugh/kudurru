import { html, type Html } from "../html.ts";
import { MIGRATIONS, MIGRATION_COMPARISON, docs } from "../content.ts";
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
  return html`<section class="sec migration-intro" aria-labelledby="move-title">
  <div class="wrap">
    <p class="label">Move over to kudurru</p>
    <h1 id="move-title" class="page-title">Keep your DNS rules.<br>Choose your next server.</h1>
    <p class="sec-lead">Coming from Pi-hole, AdGuard Home or Technitium? Compare what carries over, check the differences, then rebuild your configuration alongside the server you already run.</p>
    <div class="actions">
      <a class="btn btn-primary" href="#comparison-title">Compare features ${icon.arrow}</a>
      ${external(docs("/migrate/cutover/"), "Read the migration checklist", "btn btn-ghost")}
    </div>
    ${migrationCards()}
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
    <p class="migration-note">Without a key, kudurru supports one node, the default policy and the built-in administrator. A free key for eligible community use unlocks additional policies and up to three nodes. Business and government production use needs a commercial agreement. <a href="/license/">Review licensing and request a key ${icon.arrow}</a></p>
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
