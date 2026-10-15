import { html, type Html } from "../html.ts";
import { USE_CASES, type UseCase } from "../use-cases.ts";
import { external, icon } from "./layout.ts";

export function useCaseLinks(currentPath?: string): Html {
  return html`<ul class="use-case-links">${USE_CASES.filter((page) => page.path !== currentPath).map(
    (page) => html`<li><a href="${page.path}"><h3>${page.label} ${icon.arrow}</h3><p>${page.summary}</p></a></li>`,
  )}</ul>`;
}

export function useCases(): Html {
  return html`<section class="sec" id="use-cases" aria-labelledby="use-cases-title">
  <div class="wrap">
    <p class="sec-label use-case-label">Get started</p>
    <h2 id="use-cases-title">DNS for the network you run.</h2>
    <p class="sec-lead">Choose a starting point, see what fits, and follow the setup guide.</p>
    ${useCaseLinks()}
  </div>
</section>`;
}

export function useCase(page: UseCase): Html {
  return html`<section class="sec use-case-intro" aria-labelledby="use-case-title">
  <div class="wrap">
    <nav class="mono use-case-breadcrumb" aria-label="Breadcrumb"><a href="/">kudurru</a><span aria-hidden="true"> / </span><span aria-current="page">${page.label}</span></nav>
    <h1 class="page-title" id="use-case-title">${page.heading}</h1>
    <p class="lead">${page.intro}</p>
    <div class="actions">
      ${external(page.action.href, html`${page.action.label} ${icon.arrow}`, "btn btn-primary")}
      <a class="btn btn-ghost" href="/#dashboard">Explore the dashboard ${icon.arrow}</a>
    </div>
  </div>
</section>
<section class="sec lic-sec" aria-label="${page.label} details">
  <div class="wrap">
    ${page.sections.map((section) => html`<div class="lic-row">
      <h2>${section.title}</h2>
      <div class="lic-body">
        ${section.paragraphs.map((paragraph) => html`<p>${paragraph}</p>`)}
        ${external(section.link.href, html`${section.link.label} ${icon.arrow}`, "more")}
      </div>
    </div>`)}
    <div class="lic-row">
      <h2>Common questions</h2>
      <div class="lic-body use-case-questions">
        ${page.questions.map((item) => html`<div><h3>${item.question}</h3><p>${item.answer}</p></div>`)}
        ${page.extraLink ? external(page.extraLink.href, html`${page.extraLink.label} ${icon.arrow}`, "more") : ""}
      </div>
    </div>
  </div>
</section>
<section class="sec" aria-labelledby="related-title">
  <div class="wrap">
    <p class="sec-label use-case-label">Explore</p>
    <h2 id="related-title">More ways to run kudurru.</h2>
    ${useCaseLinks(page.path)}
  </div>
</section>`;
}
