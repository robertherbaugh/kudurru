import { DESCRIPTION, LICENSE_TEXT_PATH, REPO_URL, SITE_URL } from "./content.ts";

export const HOME_TITLE = "kudurru | Kubernetes DNS server & network ad blocking";

export interface SearchPage {
  path: string;
  title: string;
  description?: string;
}

// Escape HTML delimiters before embedding JSON in a script data block. HTML
// escaping would change the JSON; an unescaped </script> would end the block.
export function serializeJsonLd(value: unknown): string {
  return JSON.stringify(value).replace(/</g, "\\u003c").replace(/>/g, "\\u003e").replace(/&/g, "\\u0026");
}

export function structuredData(page: SearchPage, version: string): string {
  const url = SITE_URL + page.path;
  const publisher = { "@id": `${SITE_URL}/#organization` };
  const website = { "@id": `${SITE_URL}/#website` };
  const application = { "@id": `${SITE_URL}/#software` };
  const graph: Record<string, unknown>[] = [
    {
      "@type": "Organization",
      ...publisher,
      name: "kudurru project maintainers",
      url: SITE_URL + "/",
      logo: `${SITE_URL}/brand/kudurru-icon-512.png`,
      sameAs: [REPO_URL],
    },
    {
      "@type": "WebSite",
      ...website,
      url: SITE_URL + "/",
      name: "kudurru",
      description: DESCRIPTION,
      inLanguage: "en-US",
      publisher,
    },
    {
      "@type": "WebPage",
      "@id": url + "#webpage",
      url,
      name: page.title,
      description: page.description ?? DESCRIPTION,
      inLanguage: "en-US",
      isPartOf: website,
      ...(page.path === "/" ? { mainEntity: application } : { breadcrumb: { "@id": url + "#breadcrumb" } }),
    },
  ];
  if (page.path === "/") {
    graph.push({
      "@type": "SoftwareApplication",
      ...application,
      name: "kudurru",
      url: SITE_URL + "/",
      description: DESCRIPTION,
      applicationCategory: "DeveloperApplication",
      applicationSubCategory: "DNS server",
      operatingSystem: "Linux",
      softwareVersion: version,
      license: SITE_URL + LICENSE_TEXT_PATH,
      downloadUrl: REPO_URL + "/releases",
      publisher,
      // Pricing and entitlements belong to the pricing work. Do not infer
      // an Offer, price, review or rating from the community license.
    });
  } else {
    graph.push({
      "@type": "BreadcrumbList",
      "@id": url + "#breadcrumb",
      itemListElement: [
        { "@type": "ListItem", position: 1, name: "kudurru", item: SITE_URL + "/" },
        ...(page.path.startsWith("/license/") && page.path !== "/license/"
          ? [{ "@type": "ListItem", position: 2, name: "License and keys", item: SITE_URL + "/license/" }]
          : []),
        {
          "@type": "ListItem",
          position: page.path.startsWith("/license/") && page.path !== "/license/" ? 3 : 2,
          name: page.title.replace(/ (?:·|\|) kudurru$/, ""),
          item: url,
        },
      ],
    });
  }
  return serializeJsonLd({ "@context": "https://schema.org", "@graph": graph });
}
