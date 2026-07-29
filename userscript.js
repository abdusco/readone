// ==UserScript==
// @name         Readable Extractor
// @namespace    readable-extractor
// @version      1.7
// @description  Simplify page with Readability.js, download as Markdown
// @match        *://*/*
// @noframes
// @grant        GM_setClipboard
// @require      https://unpkg.com/@mozilla/readability@0.5.0/Readability.js
// @require      https://unpkg.com/turndown@7.1.2/dist/turndown.js
// @run-at       document-idle
// ==/UserScript==

(function () {
  'use strict';

  // Replaced server-side (plain string substitution) when this script is
  // served from ReadOne's /readone.user.js route. Stays literal when the
  // script is installed standalone, which is treated as "no server configured".
  const SAVE_URL = "__SAVE_URL__";
  const saveConfigured = SAVE_URL && SAVE_URL !== '__SAVE_URL__';

  async function saveArticle(art) {
    const res = await fetch(`${SAVE_URL}/api/articles`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        url: extractArchivedUrl() || location.href,
        title: art.title || '',
        byline: art.byline || '',
        siteName: art.siteName || '',
        contentHtml: art.content || '',
      }),
    });
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
  }

  const LAZY_ATTRS = ['data-src', 'data-lazy-src', 'data-original', 'data-srcset', 'data-lazy-srcset'];
  const STASHED_URL_ATTRS = ['currentsourceurl', 'data-original-src', 'data-orig-src', 'data-real-src'];

  function resolveUrl(u) {
    try { return new URL(u, location.href).href; } catch { return u; }
  }

  const delay = ms => new Promise(r => setTimeout(r, ms));

  // archive.ph (and similar snapshot/lazy sites) populate the real image src
  // and a "currentSourceUrl"-style attribute progressively as elements enter
  // the viewport. Extracting immediately on click misses anything below the
  // fold. Scroll the full page first so their own lazy-load logic fires.
  async function autoScrollFullPage() {
    const originalY = window.scrollY;
    const step = window.innerHeight * 0.8;

    let last = -1;
    let guard = 0;
    while (window.scrollY !== last && guard < 200) {
      last = window.scrollY;
      window.scrollTo(0, window.scrollY + step);
      window.dispatchEvent(new Event('scroll'));
      await delay(150);
      guard++;
    }

    // second pass: some lazy-load libs only swap the src after the element
    // has been visible for a tick, not immediately on the scroll event
    await delay(300);
    window.scrollTo(0, originalY);
    window.dispatchEvent(new Event('scroll'));
    await delay(100);
  }

  function normalizeImages(root) {
    root.querySelectorAll('img').forEach(img => {
      // Force eager loading — native lazy-load can fail to trigger inside a
      // custom scroll container since the browser's viewport-distance
      // heuristic isn't reliable for a just-inserted overlay.
      img.removeAttribute('loading');
      img.setAttribute('loading', 'eager');
      img.removeAttribute('decoding');

      // Highest priority: sites (e.g. archive.ph) that proxy/obfuscate src
      // via a <base href> pointing at a per-snapshot subdomain, but stash
      // the real permanent CDN URL in a custom attribute.
      let stashed = null;
      for (const attr of STASHED_URL_ATTRS) {
        const val = img.getAttribute(attr);
        if (val) { stashed = val; break; }
      }

      if (stashed) {
        img.setAttribute('src', resolveUrl(stashed));
        img.removeAttribute('srcset');
      } else {
        for (const attr of LAZY_ATTRS) {
          const val = img.getAttribute(attr);
          if (val && attr.includes('srcset')) img.setAttribute('srcset', val);
          else if (val) img.setAttribute('src', val);
        }
        if (img.currentSrc) img.setAttribute('src', img.currentSrc);
      }

      const src = img.getAttribute('src');
      if (src) img.setAttribute('src', resolveUrl(src));

      const srcset = img.getAttribute('srcset');
      if (srcset) {
        img.setAttribute(
          'srcset',
          srcset.split(',').map(part => {
            const [u, size] = part.trim().split(/\s+/);
            return size ? `${resolveUrl(u)} ${size}` : resolveUrl(u);
          }).join(', ')
        );
      }

      if (src && /^data:image\/(gif|svg\+xml);base64,/.test(src) && img.getAttribute('data-src')) {
        img.removeAttribute('src');
      }
    });

    // <picture><source srcset="..."> takes priority over <img src> per spec.
    // Strip sibling <source> tags once img.src is resolved to a real URL, so
    // the browser doesn't reselect a proxied avif/webp variant instead.
    root.querySelectorAll('picture source').forEach(source => {
      const picture = source.parentElement;
      const img = picture.querySelector('img');
      if (img && img.getAttribute('src')) {
        source.remove();
      }
    });
  }

  function convertBackgroundImages(root) {
    root.querySelectorAll('*').forEach(el => {
      const bg = getComputedStyle(el).backgroundImage;
      if (bg && bg !== 'none') {
        const match = bg.match(/url\((['"]?)(.*?)\1\)/);
        if (match && match[2] && !match[2].startsWith('data:') && !el.querySelector('img')) {
          const img = document.createElement('img');
          img.src = resolveUrl(match[2]);
          img.style.maxWidth = '100%';
          el.prepend(img);
        }
      }
    });
  }

  function promoteNoscriptImages(root) {
    root.querySelectorAll('noscript').forEach(ns => {
      const html = ns.textContent || ns.innerHTML;
      if (/<img/i.test(html)) {
        const wrapper = document.createElement('div');
        wrapper.innerHTML = html;
        const realImg = wrapper.querySelector('img');
        if (realImg) {
          ns.parentNode.insertBefore(realImg, ns);
        }
      }
    });
  }

  // Readability's _grabArticle() picks a "top candidate" node by paragraph/
  // text density. A hero/lead image usually lives in a near-text-free
  // wrapper (a <header>, or a <figure> alone in its own <section>) that
  // sits beside — not inside — that candidate, so it scores too low to be
  // selected or merged in as a sibling and gets silently dropped, even
  // though its src/attributes are perfectly fine. Capture it ourselves
  // before parsing so we can restore it if lost.
  //
  // Deliberately NOT falling back to <meta property="og:image">: archive.today
  // snapshots rewrite that tag to point at the archive site's own screenshot
  // thumbnail (scr.png), not the original article's image.
  function findLeadImage(root) {
    const scope = root.querySelector('article') || root;
    const selectors = [
      'header img',
      '[id*="lead-image" i] img',
      '[class*="lead-image" i] img',
      '[class*="hero-image" i] img',
      '[class*="featured-image" i] img',
    ];
    for (const sel of selectors) {
      const img = scope.querySelector(sel);
      const src = img && img.getAttribute('src');
      if (src && !src.startsWith('data:')) return { src: resolveUrl(src), alt: img.getAttribute('alt') || '' };
    }
    // Fallback: the hero image is almost always the first substantial <img>
    // before the body text, even when it isn't in a semantically-named
    // container. Skip small images so icons/avatars aren't mistaken for it.
    for (const img of scope.querySelectorAll('img')) {
      const src = img.getAttribute('src');
      if (!src || src.startsWith('data:')) continue;
      const w = img.naturalWidth || parseInt(img.getAttribute('width'), 10) || 0;
      const h = img.naturalHeight || parseInt(img.getAttribute('height'), 10) || 0;
      if ((w && w < 120) || (h && h < 120)) continue;
      return { src: resolveUrl(src), alt: img.getAttribute('alt') || '' };
    }
    return null;
  }

  function ensureLeadImage(art, lead) {
    if (!art || !art.content || !lead || art.content.includes(lead.src)) return art;
    const alt = lead.alt.replace(/"/g, '&quot;');
    art.content = `<figure><img src="${lead.src}" alt="${alt}"></figure>` + art.content;
    return art;
  }

  async function extract() {
    await autoScrollFullPage();
    promoteNoscriptImages(document.body);
    normalizeImages(document.body);
    convertBackgroundImages(document.body);

    const lead = findLeadImage(document.body);

    const docClone = document.cloneNode(true);
    const reader = new Readability(docClone, { charThreshold: 100 });
    const art = reader.parse();
    return ensureLeadImage(art, lead);
  }

  function slugify(s) {
    return (s || 'article')
      .toLowerCase()
      .replace(/[^a-z0-9]+/g, '-')
      .replace(/^-+|-+$/g, '')
      .slice(0, 80);
  }

  // archive.ph/.is/.today (and its mirror domains) embed the original URL
  // directly in their own URL, right after a timestamp/"newest"/"o/<code>"
  // path segment — pull that out so the markdown links to the real article
  // instead of the archive snapshot.
  function extractArchivedUrl() {
    const m = location.href.match(
      /^https?:\/\/(?:[a-z0-9-]+\.)?archive\.(?:ph|is|today|li|vn|fo|md)\/(?:\d{10,}|newest|o\/[^/]+)\/(https?:\/\/.+)$/i
    );
    return m ? m[1] : null;
  }

  function toMarkdown(art) {
    const td = new TurndownService({
      headingStyle: 'atx',
      codeBlockStyle: 'fenced',
      bulletListMarker: '-',
    });
    const body = td.turndown(art.content);
    const meta = [
      `# ${art.title || 'Untitled'}`,
      '',
      art.byline ? `*${art.byline}*` : null,
      art.siteName ? `Source: ${art.siteName}` : null,
      `URL: ${extractArchivedUrl() || location.href}`,
      '',
      '---',
      '',
    ].filter(Boolean).join('\n');
    return meta + '\n' + body;
  }

  function download(filename, text) {
    const blob = new Blob([text], { type: 'text/markdown;charset=utf-8' });
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    a.download = filename;
    a.click();
    URL.revokeObjectURL(url);
  }

  function renderReaderView(art) {
    const overlay = document.createElement('div');
    overlay.id = 're-overlay';
    overlay.innerHTML = `
      <style>
        #re-overlay {
          position: fixed; inset: 0; background: #fff; color: #1a1a1a;
          z-index: 2147483647; overflow-y: auto; font-family: Georgia, serif;
        }
        #re-toolbar {
          position: sticky; top: 0; background: #f5f5f5; border-bottom: 1px solid #ddd;
          padding: 10px 20px; display: flex; gap: 10px; align-items: center; z-index: 2;
          font-family: -apple-system, sans-serif;
        }
        #re-toolbar button {
          padding: 6px 14px; border: 1px solid #ccc; background: #fff; border-radius: 4px;
          cursor: pointer; font-size: 13px;
        }
        #re-toolbar button:hover { background: #eee; }
        #re-content {
          max-width: 700px; margin: 30px auto 80px; padding: 0 20px; line-height: 1.6; font-size: 18px;
        }
        #re-content img { max-width: 100%; height: auto; display: block; margin: 12px 0; }
        #re-content h1 { font-size: 32px; margin-bottom: 4px; }
        #re-meta { color: #666; font-size: 14px; margin-bottom: 20px; font-family: sans-serif; }
        #re-content pre { background: #f5f5f5; padding: 12px; overflow-x: auto; }
      </style>
      <div id="re-toolbar">
        <button id="re-close">✕ Close</button>
        <button id="re-dl-md">⬇ Download .md</button>
        <button id="re-copy-md">📋 Copy Markdown</button>
        ${saveConfigured ? '<button id="re-save">💾 Save</button>' : ''}
      </div>
      <div id="re-content">
        <h1>${art.title || ''}</h1>
        <div id="re-meta">${[art.byline, art.siteName].filter(Boolean).join(' · ')}</div>
        ${art.content}
      </div>
    `;
    document.body.appendChild(overlay);

    overlay.querySelector('#re-close').onclick = () => overlay.remove();
    overlay.querySelector('#re-dl-md').onclick = () => {
      download(`${slugify(art.title)}.md`, toMarkdown(art));
    };
    overlay.querySelector('#re-copy-md').onclick = () => {
      const md = toMarkdown(art);
      if (typeof GM_setClipboard === 'function') {
        GM_setClipboard(md, 'text');
      } else {
        navigator.clipboard.writeText(md);
      }
    };
    if (saveConfigured) {
      const saveBtn = overlay.querySelector('#re-save');
      saveBtn.onclick = async () => {
        const original = saveBtn.textContent;
        saveBtn.disabled = true;
        saveBtn.textContent = '⏳ Saving…';
        try {
          await saveArticle(art);
          saveBtn.textContent = '✅ Saved';
        } catch {
          saveBtn.textContent = '❌ Failed';
        } finally {
          setTimeout(() => { saveBtn.textContent = original; saveBtn.disabled = false; }, 2000);
        }
      };
    }
  }

  function addTrigger() {
    const btn = document.createElement('button');
    btn.textContent = '📖 Reader';
    Object.assign(btn.style, {
      position: 'fixed', bottom: '20px', right: '20px', zIndex: 2147483646,
      padding: '10px 16px', borderRadius: '24px', border: 'none',
      background: '#222', color: '#fff', fontSize: '14px', cursor: 'pointer',
      boxShadow: '0 2px 8px rgba(0,0,0,.3)', fontFamily: 'sans-serif',
    });
    btn.onclick = async () => {
      btn.disabled = true;
      const originalText = btn.textContent;
      btn.textContent = '⏳ Scanning…';
      try {
        const art = await extract();
        if (!art) {
          alert('Readability could not parse this page.');
          return;
        }
        renderReaderView(art);
      } finally {
        btn.disabled = false;
        btn.textContent = originalText;
      }
    };
    document.body.appendChild(btn);
  }

  addTrigger();
})();