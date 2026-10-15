import { html, type Html } from "../html.ts";
import {
  DOCS_URL,
  LINEAGE,
  REGISTERS,
  REPO_URL,
  ROADMAP,
  SCENARIOS,
  SHAPES,
  SHOTS,
  STAGES,
  STATUS_LABEL,
  TAGLINE,
  TOGETHER,
  TRACE,
  docs,
  installs,
  type Scenario,
  type TraceRow,
} from "../content.ts";
import { external, icon, type Assets } from "./layout.ts";


const ROMAN = ["I", "II", "III", "IV", "V", "VI", "VII", "VIII", "IX", "X"];

function micros(us: number): string {
  return us >= 1000 ? `${(us / 1000).toFixed(1)} ms` : `${us} µs`;
}

function heading(n: number, label: string, title: string, id: string): Html {
  return html`<div class="sec-head">
    <p class="sec-label"><span class="sec-num">${ROMAN[n - 1]}</span>${label}</p>
    <h2 id="${id}-title">${title}</h2>
  </div>`;
}

// One row of the hero's trace. The client script builds the same markup
// (src/client/trace.ts); keep the two in step.
export function traceRow(row: TraceRow, time: string): Html {
  const reached = STAGES.findIndex((s) => s.id === row.stage);
  return html`<li class="tr s-${row.status}">
    <span class="tr-time">${time}</span>
    <span class="tr-client">${row.client}</span>
    <span class="tr-type">${row.qtype}</span>
    <span class="tr-name">${row.qname}</span>
    <span class="tr-path" aria-hidden="true">${STAGES.map((_, i) => html`<i class="${i < reached ? "on" : i === reached ? "hit" : ""}"></i>`)}</span>
    <span class="tr-status">${STATUS_LABEL[row.status]}</span>
    <span class="tr-us">${micros(row.micros)}</span>
  </li>`;
}

function hero(version: string): Html {
  const rows = TRACE.slice(0, 9);
  // A fixed start time keeps the build reproducible; the client replaces it.
  const times = rows.map((_, i) => `21:04:${String(59 - i * 3).padStart(2, "0")}`);
  return html`<section class="hero" aria-labelledby="hero-title">
  <div class="wrap hero-grid">
    <div class="hero-copy">
      <p class="eyebrow mono"><span>Kubernetes-native DNS</span><span>Kudurru Community License</span><span>v${version} “stone”</span></p>
      <h1 id="hero-title">${TAGLINE.slice(0, -1)}<span class="stop">.</span></h1>
      <p class="lead">kudurru is a DNS server for the networks you run: authoritative zones, a resolver over every encrypted transport, blocking with client groups, and a dashboard that shows why each answer was given. One Go binary, as a single container or as a Kubernetes Deployment with CRDs.</p>
      <div class="actions">
        ${external(DOCS_URL, html`Read the docs ${icon.arrow}`, "btn btn-primary")}
        ${external(REPO_URL, html`${icon.github} Source on GitHub`, "btn btn-ghost")}
      </div>
    </div>
    <figure class="instrument" aria-label="Sample query log">
      <div class="inst-bar mono">
        <span class="inst-live"><i></i>kudurru-0</span>
        <span>query log</span>
        <span class="inst-qps"><span data-qps>20.1</span> q/s</span>
      </div>
      <div class="inst-cols mono" aria-hidden="true">
        <span>time</span><span>client</span><span>type</span><span>name</span><span>path</span><span>answer</span><span></span>
      </div>
      <ol class="trace mono" data-trace>
        ${rows.map((r, i) => traceRow(r, times[i] ?? ""))}
      </ol>
      <figcaption class="inst-foot mono">Sample traffic, rendered in your browser. Each bar follows the seven v0.1.0 stages; logging observes the result after the chain returns.</figcaption>
    </figure>
  </div>
  <div class="wrap readout mono" role="list">
    <div role="listitem"><span class="k">binary</span><span class="v">1</span><span class="d">every role, every shape</span></div>
    <div role="listitem"><span class="k">transports</span><span class="v">5</span><span class="d">Do53 · DoT · DoH · DoQ · DoH3</span></div>
    <div role="listitem"><span class="k">block lookup p99</span><span class="v">1.14 µs</span><span class="d">1,000,000 rules, 0 allocations</span></div>
    <div role="listitem"><span class="k">3 M list entries</span><span class="v">90 MB</span><span class="d">resident in the blocking benchmark</span></div>
  </div>
</section>`;
}

function lineage(): Html {
  return html`<section class="sec" id="lineage" aria-labelledby="lineage-title">
  <div class="wrap">
    ${heading(1, "Lineage", "Projects that shaped kudurru.", "lineage")}
    <p class="sec-lead">These projects inspired kudurru's DNS features, dashboard, blocking and deployment model.</p>
    <div class="ledger" role="table" aria-label="Ideas that inspired kudurru">
      <div class="ledger-row ledger-head mono" role="row">
        <span role="columnheader">Inspiration</span><span role="columnheader">From</span><span role="columnheader">What it means in kudurru v0.1.0</span>
      </div>
      ${LINEAGE.map(
        (l, i) => html`<div class="ledger-row" role="row">
        <span role="cell" class="ledger-idea"><span class="mono ledger-n">${String(i + 1).padStart(2, "0")}</span>${l.idea}</span>
        <span role="cell" class="ledger-from">${l.from.map((f, j) => html`${j > 0 ? " / " : ""}${external(f.href, f.name)}`)}</span>
        <span role="cell" class="ledger-meaning">${l.meaning}</span>
      </div>`,
      )}
    </div>
    <div class="together">
      <p class="label">How kudurru brings these ideas together</p>
      <ul>
        ${TOGETHER.map(
          (t) => html`<li><h3>${t.title}${t.release ? html` <span class="tag">${t.release}</span>` : ""}</h3><p>${t.body}</p></li>`,
        )}
      </ul>
    </div>
  </div>
</section>`;
}

function scenarioPanel(s: Scenario, i: number): Html {
  const decided = STAGES.findIndex((st) => st.id === s.decidedBy);
  const visited = new Set(s.steps.map((st) => st.stage));
  return html`<div class="scn-panel s-${s.status}" role="tabpanel" id="scn-${s.id}" aria-labelledby="scn-tab-${s.id}"${i === 0 ? "" : html` hidden`}>
    <div class="scn-query mono">
      <span class="q-proto">${s.proto}</span>
      <span class="q-from">${s.client} <span class="dim">${s.address}</span></span>
      <span class="q-ask"><span class="dim">IN</span> ${s.qtype} <b>${s.qname}</b></span>
    </div>
    <ol class="rail" aria-label="Query stages and log observer">
      ${STAGES.map((st, j) => {
        const state = j === decided ? "hit" : visited.has(st.id) ? "on" : j > decided ? "after" : "skip";
        return html`<li class="rail-cell ${state}"><span class="rail-n mono">${st.id === "log" ? "→" : String(j + 1).padStart(2, "0")}</span><span class="rail-name mono">${st.id}</span></li>`;
      })}
    </ol>
    <ol class="steps">
      ${s.steps.map(
        (st) => html`<li class="${st.stage === s.decidedBy ? "hit" : ""}"><span class="mono step-stage">${st.stage}</span><span>${st.note}</span></li>`,
      )}
    </ol>
    <div class="answer mono">
      <p class="answer-head"><span>;; ANSWER</span><span class="answer-status">${STATUS_LABEL[s.status]}</span><span>${micros(s.micros)}</span></p>
      <pre>${s.answer.join("\n")}</pre>
    </div>
  </div>`;
}

function pipeline(): Html {
  return html`<section class="sec" id="pipeline" aria-labelledby="pipeline-title">
  <div class="wrap">
    ${heading(2, "Pipeline", "Every query takes the same path.", "pipeline")}
    <p class="sec-lead">A fixed chain of stages reads lock-free snapshots of zone trees, policies, compiled lists and upstreams. The query path never touches the store, so a node keeps answering from its last good snapshot when the store is unreachable.</p>
    <div class="scn" data-tabs>
      <div class="scn-list" role="tablist" aria-label="Sample queries" aria-orientation="vertical">
        ${SCENARIOS.map(
          (s, i) => html`<button type="button" role="tab" id="scn-tab-${s.id}" aria-controls="scn-${s.id}" aria-selected="${i === 0 ? "true" : "false"}" tabindex="${i === 0 ? "0" : "-1"}" class="scn-tab s-${s.status}">
          <span class="scn-tab-label">${s.label}</span>
          <span class="mono scn-tab-q">${s.qtype} ${s.qname}</span>
        </button>`,
        )}
      </div>
      <div class="scn-view">
        ${SCENARIOS.map(scenarioPanel)}
      </div>
    </div>
    <dl class="stage-key">
      ${STAGES.map((s, i) => html`<div><dt class="mono"><span class="dim">${s.id === "log" ? "→" : String(i + 1).padStart(2, "0")}</span> ${s.id}</dt><dd>${s.does}</dd></div>`)}
    </dl>
    <p class="fine">Seven stages decide the answer; the log observer records the result after the chain returns. The fixed order also reserves local, rewrite, dnssec and postprocess for later releases. The scenarios use illustrative names and addresses.</p>
  </div>
</section>`;
}

function features(): Html {
  return html`<section class="sec" id="features" aria-labelledby="features-title">
  <div class="wrap">
    ${heading(3, "Features", "What v0.1.0 includes.", "features")}
    <div class="registers">
      ${REGISTERS.map(
        (r, i) => html`<article class="register" id="${r.id}" aria-labelledby="${r.id}-title">
        <div class="reg-side">
          <p class="reg-label mono"><span>${String(i + 1).padStart(2, "0")}</span>${r.label}</p>
          <p class="figure"><span class="fig-v">${r.figure.value}</span>${r.figure.unit ? html`<span class="fig-u">${r.figure.unit}</span>` : ""}</p>
          <p class="fig-c">${r.figure.caption}</p>
        </div>
        <div class="reg-body">
          <h3 id="${r.id}-title">${r.title}</h3>
          <p class="reg-lead">${r.lead}</p>
          <ul class="points">${r.points.map((p) => html`<li>${p}</li>`)}</ul>
          ${r.later ? html`<p class="later mono">${r.later}</p>` : ""}
          ${external(r.link.href, html`${r.link.label} ${icon.arrow}`, "more")}
        </div>
      </article>`,
      )}
    </div>
  </div>
</section>`;
}

function dashboard(assets: Assets): Html {
  return html`<section class="sec sec-paper" id="dashboard" aria-labelledby="dashboard-title">
  <div class="wrap">
    ${heading(4, "Dashboard", "A dashboard that answers why.", "dashboard")}
    <p class="sec-lead">Pi-hole's dashboard, rebuilt for more than one node. Every chart drills down into the query log, and every query in the log names the list, rule, cache state, upstream and node behind its answer. It ships inside the binary.</p>
    <div class="shots" data-tabs>
      <div class="shot-tabs mono" role="tablist" aria-label="Dashboard pages">
        ${SHOTS.map(
          (s, i) => html`<button type="button" role="tab" id="shot-tab-${s.id}" aria-controls="shot-${s.id}" aria-selected="${i === 0 ? "true" : "false"}" tabindex="${i === 0 ? "0" : "-1"}">${s.label}</button>`,
        )}
      </div>
      ${SHOTS.map(
        (s, i) => html`<figure class="shot" role="tabpanel" id="shot-${s.id}" aria-labelledby="shot-tab-${s.id}"${i === 0 ? "" : html` hidden`}>
        <div class="shot-frame"><img src="${assets.file(s.file)}" width="1280" height="1000" alt="The kudurru ${s.label} page" loading="${i === 0 ? "eager" : "lazy"}" decoding="async"></div>
        <figcaption><span class="mono">${String(i + 1).padStart(2, "0")} / ${String(SHOTS.length).padStart(2, "0")}</span>${s.caption}</figcaption>
      </figure>`,
      )}
    </div>
    <p class="shot-more">${external(docs("/guide/"), html`Tour every page in the guide ${icon.arrow}`, "more")}</p>
  </div>
</section>`;
}

function deploy(version: string): Html {
  const tabs = installs(version);
  return html`<section class="sec" id="deploy" aria-labelledby="deploy-title">
  <div class="wrap">
    ${heading(5, "Deploy", "One binary. Pick the shape.", "deploy")}
    <p class="sec-lead">In Kubernetes the apiserver is the replicated store. In Docker it is embedded SQLite. Follower mode and raft arrive later behind the same Store interface, so nothing above it changes.</p>
    <div class="deploy-grid">
      <div class="term" data-tabs>
        <div class="term-bar mono" role="tablist" aria-label="Install">
          ${tabs.map(
            (t, i) => html`<button type="button" role="tab" id="inst-tab-${t.id}" aria-controls="inst-${t.id}" aria-selected="${i === 0 ? "true" : "false"}" tabindex="${i === 0 ? "0" : "-1"}">${t.label}</button>`,
          )}
        </div>
        ${tabs.map(
          (t, i) => html`<div class="term-panel" role="tabpanel" id="inst-${t.id}" aria-labelledby="inst-tab-${t.id}"${i === 0 ? "" : html` hidden`}>
          <button type="button" class="copy mono" data-copy="inst-${t.id}-code">${icon.copy}<span>Copy</span></button>
          <pre class="mono" id="inst-${t.id}-code">${t.lines.join("\n")}</pre>
          <p class="term-note">${t.note} ${external(t.href, html`Guide ${icon.arrow}`, "more")}</p>
        </div>`,
        )}
      </div>
      <div class="shapes">
        <table>
          <caption class="label">Deployment shapes</caption>
          <thead class="mono"><tr><th scope="col">Shape</th><th scope="col">Source of truth</th><th scope="col">Config writes</th><th scope="col">Release</th></tr></thead>
          <tbody>
            ${SHAPES.map(
              (s) => html`<tr class="${s.release === "v0.1.0" ? "" : "future"}"><th scope="row">${s.name}<span class="shape-ans">Answers: ${s.answers}</span></th><td>${s.truth}</td><td>${s.writes}</td><td class="mono">${s.release}</td></tr>`,
            )}
          </tbody>
        </table>
      </div>
    </div>
    <ul class="supply mono">
      <li>linux/amd64 + linux/arm64</li>
      <li>distroless, nonroot</li>
      <li>cosign keyless signatures</li>
      <li>SBOM + SLSA provenance</li>
      <li>deb · rpm · apk with systemd</li>
    </ul>
  </div>
</section>`;
}

function name(assets: Assets): Html {
  return html`<section class="sec sec-name" id="name" aria-labelledby="name-title">
  <div class="wrap name-grid">
    <div class="name-mark">
      <img src="${assets.file("kudurru-mark-clay.png")}" width="1024" height="1024" alt="" loading="lazy">
    </div>
    <div class="name-copy">
      <p class="sec-label"><span class="sec-num">VI</span>The name</p>
      <h2 id="name-title"><span class="say">koo-DOO-roo</span></h2>
      <p>A kudurru was a Babylonian boundary stone that recorded a land grant and the rights attached to it. The name reflects what a DNS zone represents: authority over a defined space.</p>
      <p>The original stone was kept in a temple, while the landholder held a clay copy—a fitting inspiration for authoritative DNS and the copies shared through zone transfers. Our release names follow that story: stone, clay copies, temple.</p>
    </div>
  </div>
</section>`;
}

function roadmap(): Html {
  return html`<section class="sec" id="roadmap" aria-labelledby="roadmap-title">
  <div class="wrap">
    ${heading(7, "Roadmap", "stone, clay copies, temple.", "roadmap")}
    <ol class="stones">
      ${ROADMAP.map(
        (r, i) => html`<li class="stone${i === 0 ? " now" : ""}">
        <p class="stone-v mono"><span>${r.version}</span>${i === 0 ? html`<span class="tag">this release</span>` : ""}</p>
        <h3>${r.name}</h3>
        <p class="stone-m">${r.meaning}</p>
        <ul>${r.items.map((it) => html`<li>${it}</li>`)}</ul>
      </li>`,
      )}
    </ol>
    <p class="fine">Milestones and exit criteria: ${external(`${REPO_URL}/blob/main/docs/architecture.md#13-roadmap`, "design record, section 13")}.</p>
  </div>
</section>`;
}

function closing(): Html {
  return html`<section class="closing" aria-labelledby="closing-title">
  <div class="wrap closing-grid">
    <h2 id="closing-title">Start with one container.<br>Grow into a cluster.</h2>
    <div class="actions">
      ${external(docs("/start/"), html`Get started ${icon.arrow}`, "btn btn-primary")}
      ${external(REPO_URL, html`${icon.github} Star on GitHub`, "btn btn-ghost")}
    </div>
  </div>
</section>`;
}

export function home(assets: Assets, version: string): Html {
  return html`${hero(version)}
${lineage()}
${pipeline()}
${features()}
${dashboard(assets)}
${deploy(version)}
${name(assets)}
${roadmap()}
${closing()}`;
}

export function notFound(): Html {
  return html`<section class="hero lost" aria-labelledby="lost-title">
  <div class="wrap">
    <p class="eyebrow mono"><span>404</span><span>NXDOMAIN</span></p>
    <h1 id="lost-title">No such name<span class="stop">.</span></h1>
    <pre class="mono lost-dig">;; -&gt;&gt;HEADER&lt;&lt;- opcode: QUERY, status: NXDOMAIN
;; flags: qr aa rd ra; QUERY: 1, ANSWER: 0, AUTHORITY: 1</pre>
    <div class="actions">
      <a class="btn btn-primary" href="/">Back to kudurru.io ${icon.arrow}</a>
      ${external(DOCS_URL, html`Search the docs ${icon.out}`, "btn btn-ghost")}
    </div>
  </div>
</section>`;
}

export function licenses(names: { name: string; file: string }[]): Html {
  return html`<section class="sec" aria-labelledby="lic-title">
  <div class="wrap narrow">
    <h1 id="lic-title" class="page-title">Font licenses</h1>
    <p>This site uses Inter and JetBrains Mono, self-hosted. Both are licensed under the SIL Open Font License 1.1.</p>
    <ul class="points">${names.map((n) => html`<li><a href="${n.file}">${n.name}</a></li>`)}</ul>
    <p>Copyright 2026 kudurru project maintainers. This marketing website has a separate ${external(`${REPO_URL}/blob/main/www/LICENSE.md`, "source-viewing license")}: inspection is permitted; modification, redistribution and hosting require prior written authorization. The fonts retain their own licenses.</p>
  </div>
</section>`;
}
