// kudurru docs: the menu toggle on small screens, copy buttons on code
// blocks, and a client-side search over search-index.json (loaded the first
// time the search box gets focus). No dependencies, nothing from a CDN.
(() => {
  'use strict';

  const root = document.documentElement;
  const control = document.querySelector('.theme-switch');
  if (control) {
    const options = control.querySelectorAll('input[type="radio"]');
    const system = matchMedia('(prefers-color-scheme: dark)');
    const colors = document.querySelectorAll('meta[name="theme-color"][data-scheme]');
    const effective = () => root.dataset.theme ?? (system.matches ? 'dark' : 'light');
    const announce = () => document.dispatchEvent(new CustomEvent('kudurru:themechange', { detail: effective() }));
    const update = (theme) => {
      const before = effective();
      if (theme === 'light' || theme === 'dark') root.dataset.theme = theme;
      else delete root.dataset.theme;
      for (const option of options) option.checked = option.value === theme;
      for (const meta of colors) {
        const scheme = meta.dataset.scheme;
        meta.media = theme === 'system' ? `(prefers-color-scheme: ${scheme})` : theme === scheme ? 'all' : 'not all';
      }
      if (effective() !== before) announce();
    };
    update(root.dataset.theme ?? 'system');
    control.addEventListener('change', (event) => {
      if (!(event.target instanceof HTMLInputElement)) return;
      const theme = event.target.value;
      update(theme);
      try {
        if (theme === 'system') localStorage.removeItem('kudurru-theme');
        else localStorage.setItem('kudurru-theme', theme);
      } catch {}
    });
    system.addEventListener('change', () => { if (!root.dataset.theme) announce(); });
    control.hidden = false;
  }

  const scrollers = document.querySelectorAll('.table-wrap, pre.mermaid');
  if (scrollers.length && typeof ResizeObserver !== 'undefined') {
    const reflow = new ResizeObserver((entries) => {
      for (const { target } of entries) {
        if (target.scrollWidth > target.clientWidth) target.tabIndex = 0;
        else target.removeAttribute('tabindex');
      }
    });
    for (const scroller of scrollers) reflow.observe(scroller);
  }

  const sidebar = document.querySelector('.sidebar__tree');
  if (sidebar) {
    const narrow = window.matchMedia('(max-width: 900px)');
    const syncSidebar = () => { sidebar.open = !narrow.matches; };
    syncSidebar();
    narrow.addEventListener('change', syncSidebar);
  }

  const toggle = document.querySelector('.nav-toggle');
  const nav = document.getElementById('site-nav');
  if (toggle && nav) {
    const setOpen = (open) => {
      toggle.setAttribute('aria-expanded', String(open));
      document.body.classList.toggle('nav-open', open);
    };
    toggle.addEventListener('click', () => setOpen(toggle.getAttribute('aria-expanded') !== 'true'));
    document.addEventListener('keydown', (event) => {
      if (event.key !== 'Escape' || toggle.getAttribute('aria-expanded') !== 'true') return;
      if (nav.contains(document.activeElement)) toggle.focus();
      setOpen(false);
    });
  }

  for (const pre of document.querySelectorAll('.prose pre:not(.mermaid)')) {
    if (!navigator.clipboard) break;
    const button = document.createElement('button');
    button.type = 'button';
    button.className = 'copy';
    button.textContent = 'Copy';
    button.addEventListener('click', async () => {
      try {
        await navigator.clipboard.writeText(pre.innerText.replace(/\n$/, ''));
        button.textContent = 'Copied';
      } catch {
        button.textContent = 'Failed';
      }
      setTimeout(() => { button.textContent = 'Copy'; }, 1500);
    });
    const wrap = pre.parentElement && pre.parentElement.classList.contains('highlight') ? pre.parentElement : pre;
    wrap.classList.add('has-copy');
    wrap.appendChild(button);
  }

  const input = document.getElementById('search-input');
  const results = document.getElementById('search-results');
  const meta = document.querySelector('meta[name="kudurru-search-index"]');
  if (!input || !results || !meta) return;

  let index = null;
  let loading = null;
  const load = () => {
    if (!loading) {
      loading = fetch(meta.content)
        .then((r) => (r.ok ? r.json() : []))
        .then((items) => {
          index = items.map((i) => ({ ...i, lt: i.t.toLowerCase(), lc: i.c.toLowerCase() }));
        })
        .catch(() => { index = []; });
    }
    return loading;
  };

  const snippet = (item, terms) => {
    const at = item.lc.indexOf(terms[0]);
    if (at < 0) return item.c.slice(0, 140);
    const start = Math.max(0, at - 50);
    return (start > 0 ? '…' : '') + item.c.slice(start, start + 160).replace(/\s+/g, ' ') + '…';
  };

  const search = (q) => {
    const terms = q.toLowerCase().split(/\s+/).filter(Boolean);
    if (!terms.length || !index) return [];
    const hits = [];
    for (const item of index) {
      let score = 0;
      let all = true;
      for (const t of terms) {
        const inTitle = item.lt.includes(t);
        const inBody = item.lc.includes(t);
        if (!inTitle && !inBody) { all = false; break; }
        score += (inTitle ? 10 : 0) + (inBody ? 1 : 0);
      }
      if (all) hits.push({ item, score });
    }
    hits.sort((a, b) => b.score - a.score || a.item.t.localeCompare(b.item.t));
    return hits.slice(0, 12).map((h) => ({ ...h.item, snippet: snippet(h.item, terms) }));
  };

  const render = (hits, q) => {
    results.replaceChildren();
    if (!q) { results.hidden = true; return; }
    if (!hits.length) {
      const li = document.createElement('li');
      li.className = 'search__empty';
      li.textContent = 'No pages match.';
      results.append(li);
    }
    for (const h of hits) {
      const li = document.createElement('li');
      const a = document.createElement('a');
      a.href = h.u;
      const title = document.createElement('strong');
      title.textContent = h.t;
      a.append(title);
      if (h.s) {
        const section = document.createElement('span');
        section.className = 'search__section';
        section.textContent = h.s;
        a.append(section);
      }
      const p = document.createElement('span');
      p.className = 'search__snippet';
      p.textContent = h.snippet;
      a.append(p);
      li.append(a);
      results.append(li);
    }
    results.hidden = false;
  };

  input.addEventListener('focus', load, { once: true });
  input.addEventListener('input', async () => {
    await load();
    const q = input.value.trim();
    render(search(q), q);
  });
  input.addEventListener('keydown', (e) => {
    if (e.key === 'Escape') { input.value = ''; render([], ''); input.blur(); }
    if (e.key === 'Enter') {
      const first = results.querySelector('a');
      if (first) window.location.href = first.href;
    }
  });
  document.addEventListener('keydown', (e) => {
    if (e.key === '/' && document.activeElement !== input && !(e.target instanceof HTMLInputElement)) {
      e.preventDefault();
      input.focus();
    }
  });
  document.addEventListener('click', (e) => {
    if (!e.target.closest('.search')) results.hidden = true;
  });
})();
