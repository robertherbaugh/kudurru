import { html, type Html } from "../html.ts";
import { COMMERCIAL_COMPARISON, COMMERCIAL_PLANS, ENTERPRISE_CONTACT_URL, ENTERPRISE_FEATURES, LICENSE_FREE_NODES, VOLUME_PRICING, docs } from "../content.ts";
import { annualPrice, usd } from "../pricing.ts";
import { external, icon } from "./layout.ts";

function calculator(): Html {
  const price = annualPrice("enterprise", 5)!;
  return html`<aside class="enterprise-pricing" data-pricing aria-labelledby="enterprise-calculator-title">
    <p class="label">Pricing calculator</p>
    <h2 id="enterprise-calculator-title">A platform fee.<br>Plus your nodes.</h2>
    <div class="pricing-controls">
      <fieldset class="pricing-editions">
        <legend>Commercial edition</legend>
        ${Object.entries(COMMERCIAL_PLANS).map(([id, plan]) => html`<label><input type="radio" name="pricing-edition" value="${id}" ${id === "enterprise" ? html`checked` : ""}><span>${plan.name}</span></label>`)}
      </fieldset>
      <label class="pricing-nodes" for="pricing-nodes">Licensed production nodes
        <input id="pricing-nodes" data-pricing-nodes type="number" min="1" step="1" value="5" inputmode="numeric" required aria-describedby="pricing-help pricing-message">
      </label>
      <p class="fine" id="pricing-help">${VOLUME_PRICING.discountFrom}–${VOLUME_PRICING.quoteFrom - 1} nodes: ${VOLUME_PRICING.discount * 100}% off node fees. ${VOLUME_PRICING.quoteFrom}+: individual quote.</p>
    </div>
    <p class="nojs-note fine">Example: Enterprise with 5 production nodes. Enable JavaScript to calculate your deployment.</p>
    <div aria-live="polite" aria-atomic="true">
      <dl class="enterprise-fees" data-pricing-rows>
        <div><dt>Annual platform fee</dt><dd data-price="platform">${usd(price.platform)}</dd></div>
        <div><dt data-price="node-label">5 × ${usd(price.perNode)} per node</dt><dd data-price="nodes">${usd(price.nodeFee)}</dd></div>
      </dl>
      <div class="pricing-total" data-pricing-total>
        <p>Total annual license</p>
        <p data-pricing-result>${usd(price.total)}</p>
      </div>
      <p class="fine" id="pricing-message" data-pricing-message>Equivalent to ${usd(price.total / 12, true)}/month, billed annually.</p>
    </div>
    <p class="fine">USD. Software license only; support is separate.</p>
    <a class="more" href="${ENTERPRISE_CONTACT_URL}">Discuss your deployment ${icon.arrow}</a>
  </aside>`;
}

export function enterprise(): Html {
  return html`<section class="sec enterprise-intro" aria-labelledby="enterprise-title">
  <div class="wrap enterprise-hero">
    <div>
      <p class="label">Commercial licensing</p>
      <h1 id="enterprise-title" class="page-title">Your DNS.<br>Your infrastructure.<br>Your team in control.</h1>
      <p class="reg-lead">Run kudurru on your own infrastructure. Choose Commercial Core for production DNS, or Enterprise for the tools to manage access, review changes and operate across nodes.</p>
      <p class="enterprise-summary">One annual platform fee per legal organization, plus your licensed production nodes. Deploy with Kubernetes and CRDs, or with Docker and Compose.</p>
      <div class="actions">
        <a class="btn btn-primary" href="#enterprise-plans-title">Compare plans ${icon.arrow}</a>
        ${external(docs("/enterprise/"), "Read the enterprise docs", "btn btn-ghost")}
      </div>
      <p class="enterprise-continuity"><strong>DNS keeps answering.</strong> Keys are verified locally. A missing, invalid or expired key never stops DNS queries; it can restrict management features.</p>
    </div>
    ${calculator()}
  </div>
</section>
<section class="sec enterprise-section" aria-labelledby="enterprise-plans-title">
  <div class="wrap">
    <div class="sec-head">
      <p class="sec-label">Plans and pricing</p>
      <h2 id="enterprise-plans-title">Start with DNS.<br>Add what your team needs.</h2>
    </div>
    <p class="sec-lead">Two commercial editions, billed annually in USD. No DNS query, zone, CPU core or user limits on either paid plan. Support contracts are separate.</p>
    <div class="enterprise-plans">
      <article class="enterprise-plan">
        <p class="label">For eligible noncommercial use</p>
        <h3>Community / Homelab</h3>
        <p class="plan-price">Free</p>
        <p class="plan-description">Personal, household, homelab and qualifying nonprofit use under the Kudurru Community License.</p>
        <ul class="points">
          <li>Core DNS and dashboard without a key</li>
          <li>A free key unlocks up to ${LICENSE_FREE_NODES} nodes and enterprise features</li>
          <li>Business and government production require a commercial license</li>
        </ul>
        <a class="more" href="/license/">Get a free key ${icon.arrow}</a>
      </article>
      ${Object.entries(COMMERCIAL_PLANS).map(([id, plan]) => html`<article class="enterprise-plan ${id === "enterprise" ? "enterprise-plan-featured" : ""}">
        <p class="label">${id === "core" ? "Commercial rights for core DNS" : "Commercial rights + enterprise modules"}</p>
        <h3>${plan.name}</h3>
        <p class="plan-price">${usd(plan.platform)}<span>/year platform fee</span></p>
        <p class="plan-node-price">+ ${usd(plan.perNode)} / node / year</p>
        <p class="plan-description">${id === "core" ? "Production rights for the core DNS server and dashboard, for businesses and government organizations." : "Everything in Commercial Core, plus all currently shipped enterprise modules in one subscription."}</p>
        <ul class="points">
          ${id === "core" ? html`<li>Authoritative DNS and forwarding resolver</li><li>Blocking, encrypted DNS and dashboard</li><li>Multiple nodes at your licensed count</li>` : html`<li>OIDC, advanced RBAC and audit exports</li><li>Long-term analytics, advanced policies and unblock requests</li><li>dnsdist, keepalived and enterprise integrations</li>`}
        </ul>
        <p class="fine">${usd(plan.platform + plan.perNode)}/year for 1 production node.</p>
        <a class="more" href="${ENTERPRISE_CONTACT_URL}">Discuss ${plan.name} ${icon.arrow}</a>
      </article>`)}
    </div>
    <p class="fine">Commercial purchases require an accepted agreement and Order Form. Named-module agreements remain available for exceptional deployments.</p>
    <div class="shapes lic-table enterprise-comparison" role="region" aria-labelledby="enterprise-comparison-caption" tabindex="0">
      <table>
        <caption id="enterprise-comparison-caption">Compare the editions</caption>
        <thead><tr><th scope="col">Capability</th><th scope="col">Community / Homelab</th><th scope="col">Commercial Core</th><th scope="col">Enterprise</th></tr></thead>
        <tbody>${COMMERCIAL_COMPARISON.map((row) => html`<tr><th scope="row">${row.what}</th><td>${row.community}</td><td>${row.core}</td><td>${row.enterprise}</td></tr>`)}</tbody>
      </table>
    </div>
    <p class="fine">Community features above assume an eligible free key. Without a key, one node and the default policy are supported. The Kudurru Community License specifically excludes business and government production use.</p>
  </div>
</section>
<section class="sec enterprise-section" aria-labelledby="enterprise-scale-title">
  <div class="wrap">
    <div class="lic-row">
      <div>
        <p class="label">Annual license examples</p>
        <h2 id="enterprise-scale-title">As your network grows.</h2>
        <p class="enterprise-summary">Platform fee + (per-node fee × licensed nodes). The platform fee stays the same across your organization’s clusters.</p>
      </div>
      <div>
        <div class="shapes lic-table" role="region" aria-labelledby="enterprise-scale-caption" tabindex="0">
          <table>
            <caption id="enterprise-scale-caption">Total annual software license in USD</caption>
            <thead><tr><th scope="col">Licensed nodes</th><th scope="col">Commercial Core / year</th><th scope="col">Enterprise / year</th></tr></thead>
            <tbody>${[1, 3, 5, 10, VOLUME_PRICING.discountFrom, VOLUME_PRICING.quoteFrom].map((nodes) => html`<tr><th scope="row">${nodes === VOLUME_PRICING.quoteFrom ? `${nodes}+` : nodes}</th>${(["core", "enterprise"] as const).map((edition) => {
              const price = annualPrice(edition, nodes);
              return html`<td>${price ? usd(price.total) : html`<a href="${ENTERPRISE_CONTACT_URL}">Contact us</a>`}</td>`;
            })}</tr>`)}</tbody>
          </table>
        </div>
        <p class="fine">${VOLUME_PRICING.discountFrom}–${VOLUME_PRICING.quoteFrom - 1} nodes receive ${VOLUME_PRICING.discount * 100}% off the entire per-node component; the platform fee is unchanged. Deployments of ${VOLUME_PRICING.quoteFrom} or more nodes are quoted individually.</p>
      </div>
    </div>
  </div>
</section>
<section class="sec enterprise-section" aria-labelledby="enterprise-features-title">
  <div class="wrap">
    <div class="sec-head">
      <p class="sec-label">Included with Enterprise</p>
      <h2 id="enterprise-features-title">Built for the people<br>who run the network.</h2>
    </div>
    <p class="sec-lead">All currently shipped enterprise modules are included in one Enterprise subscription. These capabilities are available today.</p>
    <div class="enterprise-features">
      ${ENTERPRISE_FEATURES.map((feature, i) => html`<article>
        <p class="mono enterprise-number" aria-hidden="true">0${i + 1}</p>
        <h3>${feature.title}</h3>
        <p>${feature.body}</p>
        ${external(docs(feature.href), html`Read the docs ${icon.arrow}`, "more")}
      </article>`)}
    </div>
  </div>
</section>
<section class="sec enterprise-section" aria-labelledby="enterprise-terms-title">
  <div class="wrap">
    <div class="lic-row">
      <h2 id="enterprise-terms-title">Simple commercial terms</h2>
      <div class="lic-body">
        <p><strong>One organization, one platform fee.</strong> Pay annually in USD for your edition and the aggregate licensed production nodes across the clusters and environments covered by your Order Form. Monthly equivalents are illustrative; subscriptions are billed annually.</p>
        <p><strong>Service providers and MSPs.</strong> Pooled client deployments need separate service-provider authorization in the Order Form. Use either commercial edition with that authorization; affiliate and government scope must also be expressly covered.</p>
        <p><strong>Evaluation, staging and disaster recovery.</strong> Isolated evaluation is free under the Community License. Continuously running staging, DR and failover nodes must be explicitly covered by the Order Form; extra running nodes are not included by default.</p>
      </div>
    </div>
    <div class="lic-row">
      <h2 id="enterprise-nodes-title">What counts as a node?</h2>
      <div class="lic-body">
        <p>A node is one independently running kudurru DNS server instance or replica, on a physical host, VM, container or Kubernetes workload. Three running Kubernetes replicas count as three nodes, even on the same host. DNS clients are not nodes.</p>
        <p>Stored images, stopped standbys and backups do not count. Separate dnsdist and keepalived processes are not additional kudurru nodes merely by existing.</p>
        <p>The commercial agreement permits up to 24 hours of temporary overlap for a failed-node replacement or an ordinary rolling upgrade, without increasing steady-state capacity.</p>
      </div>
    </div>
    <div class="lic-row">
      <h2 id="enterprise-support-title">Support, when you need it</h2>
      <div class="lic-body">
        <p>Optional business-hours assistance starts at $1,200/year, subject to availability and a separate agreement. Software license prices include no contractual support, 24/7 coverage or guaranteed response times.</p>
        <p>Support scope, response commitments and any SLA are defined in your Order Form or incorporated support schedule.</p>
      </div>
    </div>
    <div class="lic-row">
      <h2 id="enterprise-offline-title">Keys stay local.<br>DNS keeps answering.</h2>
      <div class="lic-body">
        <p>Nodes verify license keys locally with a signature check, without contacting a license server. DNS queries continue regardless of key validity. A missing or expired key can limit management features; technical grace periods do not extend the commercial subscription.</p>
        <p>Read the ${external(docs("/project/commercial-license/"), "commercial agreement")} for the terms and the ${external(docs("/enterprise/"), "enterprise docs")} for deployment and licensing details.</p>
      </div>
    </div>
  </div>
</section>
<section class="closing enterprise-contact" aria-labelledby="enterprise-contact-title">
  <div class="wrap closing-grid">
    <div>
      <p class="label">Start with your deployment</p>
      <h2 id="enterprise-contact-title">Let’s size it together.</h2>
      <p>Tell us your organization, node count, deployment model and the features you need. Include staging, disaster recovery, support and any service-provider requirements.</p>
    </div>
    <a class="btn btn-primary" href="${ENTERPRISE_CONTACT_URL}">Discuss your license ${icon.arrow}</a>
  </div>
</section>`;
}
