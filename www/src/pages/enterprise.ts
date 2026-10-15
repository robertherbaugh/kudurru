import { html, type Html } from "../html.ts";
import { ENTERPRISE_CONTACT_URL, ENTERPRISE_FEATURES, LICENSE_FREE_NODES, docs } from "../content.ts";
import { external, icon } from "./layout.ts";

export function enterprise(): Html {
  return html`<section class="sec enterprise-intro" aria-labelledby="enterprise-title">
  <div class="wrap enterprise-hero">
    <div>
      <p class="label">kudurru Enterprise</p>
      <h1 id="enterprise-title" class="page-title">Your DNS.<br>Your infrastructure.<br>Your team in control.</h1>
      <p class="reg-lead">Run kudurru on your own infrastructure with commercial production rights and the tools to manage access, review changes and operate across nodes.</p>
      <p class="enterprise-summary">The same DNS server, with enterprise entitlements for the way your organization works. Deploy with Kubernetes and CRDs, or with Docker and Compose.</p>
      <div class="actions">
        <a class="btn btn-primary" href="${ENTERPRISE_CONTACT_URL}">Discuss Enterprise ${icon.arrow}</a>
        ${external(docs("/enterprise/"), "Read the enterprise docs", "btn btn-ghost")}
      </div>
    </div>
    <aside class="enterprise-pricing" aria-labelledby="enterprise-pricing-title">
      <p class="label">Proposed pricing · not final</p>
      <h2 id="enterprise-pricing-title">A platform fee.<br>Plus your nodes.</h2>
      <p>The proposed model combines a platform fee with a fee for each licensed node.</p>
      <dl class="enterprise-fees">
        <div><dt>Platform fee</dt><dd>TBD <span>amount and billing period</span></dd></div>
        <div><dt>Per-node fee</dt><dd>TBD <span>per node, per billing period</span></dd></div>
      </dl>
      <p class="enterprise-formula mono">Total = platform fee<br>+ (per-node fee × licensed nodes)</p>
      <p class="fine">All amounts are placeholders. Pricing, billing periods, minimum commitments and included entitlements are still being finalized. Your accepted Order Form sets the final fees and scope.</p>
      <a class="more" href="${ENTERPRISE_CONTACT_URL}">Talk through your deployment ${icon.arrow}</a>
    </aside>
  </div>
</section>
<section class="sec enterprise-section" aria-labelledby="enterprise-features-title">
  <div class="wrap">
    <div class="sec-head">
      <p class="sec-label">Enterprise capabilities</p>
      <h2 id="enterprise-features-title">Built for the people<br>who run the network.</h2>
    </div>
    <p class="sec-lead">These capabilities are available today. Your Order Form identifies the Enterprise plan or the named modules included in your purchase.</p>
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
<section class="sec enterprise-section" aria-labelledby="enterprise-scope-title">
  <div class="wrap">
    <div class="lic-row">
      <h2 id="enterprise-scope-title">Choose your scope</h2>
      <div class="lic-body">
        <p><strong>Commercial Core</strong> grants business or government production rights for the core DNS server and dashboard, within the node count and scope of your agreement. It includes authoritative zones, forwarding, encrypted transports, blocking and short-term statistics.</p>
        <p><strong>Enterprise</strong> adds the enterprise modules and advanced entitlements identified in your Order Form. A Commercial Core purchase does not automatically include those modules.</p>
        <p>Support scope, response times and any SLA are agreed separately in the Order Form or an incorporated support schedule. MSP, affiliate and government use must be expressly covered by the Order Form.</p>
        <p>Eligible personal, homelab and qualifying nonprofit use remains free under the Community License, with a free key for up to ${LICENSE_FREE_NODES} nodes. <a href="/license/">Compare licenses and keys ${icon.arrow}</a></p>
      </div>
    </div>
    <div class="lic-row">
      <h2 id="enterprise-nodes-title">What counts as a node?</h2>
      <div class="lic-body">
        <p>A node is one independently running kudurru DNS server instance or replica. Three running Kubernetes replicas count as three nodes, even on the same host. DNS clients are not nodes.</p>
        <p>Production nodes count across the environments and clusters covered by your agreement. Stored images, stopped standbys and backups do not count. Separate dnsdist and keepalived processes are not additional kudurru nodes merely by existing.</p>
        <p>Include staging, disaster recovery and failover needs when discussing your deployment. The Order Form defines those entitlements; extra running nodes are not included by default.</p>
      </div>
    </div>
    <div class="lic-row">
      <h2 id="enterprise-offline-title">Keys stay local</h2>
      <div class="lic-body">
        <p>Nodes verify license keys locally with a signature check. There is no license-server connection in the DNS path. DNS queries continue to be answered regardless of key state; a missing or expired key can limit management features.</p>
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
      <p>Tell us your organization, node count, deployment model and the features you need.</p>
    </div>
    <a class="btn btn-primary" href="${ENTERPRISE_CONTACT_URL}">Discuss Enterprise ${icon.arrow}</a>
  </div>
</section>`;
}
