// ==UserScript==
// @name         Readability by readone
// @namespace    readable-extractor
// @version      2.2
// @description  Simplify page with Readability.js, download as Markdown
// @match        *://*/*
// @noframes
// @grant        GM_setClipboard
// @grant        GM_xmlhttpRequest
// @grant        GM_getValue
// @grant        GM_setValue
// @connect      *
// @run-at       document-idle
// ==/UserScript==

(function () {
  'use strict';

  // Replaced server-side (plain string substitution) when this script is
  // served from ReadOne's /readone.user.js route. Stays literal when the
  // script is installed standalone, which is treated as "no server configured".
  const SAVE_URL = "__SAVE_URL__";
  const saveConfigured = SAVE_URL && SAVE_URL !== '__SAVE_URL__';

  // Readability/Turndown/JSZip used to be @require'd, which makes Tampermonkey
  // execute all three on every single page load matching @match *://*/*, even
  // pages the Reader button is never clicked on. Load them on demand instead,
  // the first time the button is used, and cache the loading promise so a
  // second click doesn't reload them.
  let librariesPromise = null;
  function loadScript(url) {
    return new Promise((resolve, reject) => {
      const s = document.createElement('script');
      s.src = url;
      s.onload = () => { s.remove(); resolve(); };
      s.onerror = () => { s.remove(); reject(new Error(`failed to load ${url}`)); };
      (document.head || document.documentElement).appendChild(s);
    });
  }
  function ensureLibraries() {
    if (!librariesPromise) {
      librariesPromise = Promise.all([
        loadScript('https://unpkg.com/@mozilla/readability@0.5.0/Readability.js'),
        loadScript('https://unpkg.com/turndown@7.1.2/dist/turndown.js'),
        loadScript('https://unpkg.com/jszip@3.10.1/dist/jszip.min.js'),
      ]);
    }
    return librariesPromise;
  }

  // Plain fetch() can't read cross-origin image bytes unless the origin
  // opts in with Access-Control-Allow-Origin — which most image CDNs don't
  // send, since that header is for script-readability, not <img> display.
  // GM_xmlhttpRequest is a privileged Tampermonkey API that bypasses that
  // restriction entirely (the standard way userscripts fetch cross-origin
  // resources), using the browser's real network stack — same UA and
  // cookies a normal page load would send. Referer isn't sent automatically
  // though, so it's set explicitly for CDNs that hotlink-check on it.
  function gmFetchBlob(url) {
    return new Promise((resolve, reject) => {
      GM_xmlhttpRequest({
        method: 'GET',
        url,
        responseType: 'blob',
        headers: { Referer: location.href },
        onload: res => (res.status >= 200 && res.status < 300) ? resolve(res.response) : reject(new Error(`HTTP ${res.status}`)),
        onerror: () => reject(new Error('network error')),
      });
    });
  }

  // Finds the live, already-loaded <img> in the real document matching src
  // (the wrapper buildImageAssets iterates over is a detached clone parsed
  // from contentHtml — its <img> elements were never actually loaded), and
  // re-encodes its already-decoded pixels as a PNG via canvas. This is the
  // fallback for images gmFetchBlob can't fetch (some sites' bot mitigation
  // blocks XHR-style requests for a resource that loaded fine as a normal
  // on-page <img>) — no network request needed, since the browser already
  // has the pixels for display. It only works if the canvas isn't
  // "tainted": same-origin images are always fine; cross-origin images need
  // the origin to send permissive CORS headers, which the ones blocking
  // gmFetchBlob typically don't, so this is best-effort on top of
  // best-effort. Static PNG only — an animated GIF/WebP loses its animation,
  // since canvas only ever holds the currently-displayed frame.
  async function canvasCaptureBlob(src) {
    const img = Array.from(document.images).find(el => el.currentSrc === src || el.src === src);
    if (!img || !img.complete || !img.naturalWidth) return null;

    const canvas = document.createElement('canvas');
    canvas.width = img.naturalWidth;
    canvas.height = img.naturalHeight;
    canvas.getContext('2d').drawImage(img, 0, 0);

    try {
      return await new Promise((resolve, reject) => {
        canvas.toBlob(blob => (blob ? resolve(blob) : reject(new Error('empty canvas blob'))));
      });
    } catch {
      return null; // tainted canvas (cross-origin image without permissive CORS) — nothing more we can do
    }
  }

  // Downloads every remote <img> in contentHtml from the browser (so it goes
  // out with the same cookies/UA/referrer that already got the page itself
  // past any bot-blocking) and packs them into a zip, alongside a map.json
  // recording which original URL each entry came from. The server reads that
  // mapping to rewrite <img src> itself, so this leaves contentHtml
  // untouched — the zip is the only thing that knows about local paths. This
  // makes the images available to EPUB export later even if the server can't
  // reach the origin site directly. Images that fail to fetch (and can't be
  // canvas-captured either) are simply left out of the mapping, same as
  // before this feature existed.
  async function buildImageAssets(contentHtml) {
    await ensureLibraries();
    const wrapper = document.createElement('div');
    wrapper.innerHTML = contentHtml;
    const zip = new unsafeWindow.JSZip();
    const assetMap = {};
    let count = 0;

    for (const img of wrapper.querySelectorAll('img')) {
      const src = img.getAttribute('src');
      if (!src || !/^https?:/i.test(src) || assetMap[src]) continue;

      let blob = null;
      try {
        blob = await gmFetchBlob(src);
      } catch {
        blob = await canvasCaptureBlob(src);
      }
      if (!blob) continue; // both the direct fetch and the canvas fallback failed — leave the original remote src

      // blob.type can carry a trailing parameter (e.g. "image/jpeg;charset=utf-8")
      // when a server sends a malformed Content-Type on an image response —
      // strip it before deriving the extension, or the zip entry ends up
      // named "images/0.jpg;charset=utf-8".
      const mime = blob.type.split(';')[0].trim();
      const ext = (mime.split('/')[1] || 'jpg').replace('jpeg', 'jpg').split('+')[0];
      const name = `images/${count}.${ext}`;
      zip.file(name, blob);
      assetMap[src] = name;
      count++;
    }

    if (count === 0) return { assetsZip: null };
    zip.file('map.json', JSON.stringify(assetMap));
    return { assetsZip: await zip.generateAsync({ type: 'blob' }) };
  }

  async function saveArticle(art) {
    const { assetsZip } = await buildImageAssets(art.content || '');

    const form = new FormData();
    form.append('metadata', JSON.stringify({
      url: extractArchivedUrl() || location.href,
      title: art.title || '',
      byline: art.byline || '',
      siteName: art.siteName || '',
      contentHtml: art.content || '',
    }));
    if (assetsZip) form.append('assets', assetsZip, 'assets.zip');

    const res = await fetch(`${SAVE_URL}/api/articles`, { method: 'POST', body: form });
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

  // Readability's parser runs against a cloned DOM (see extract()), which
  // clones attributes verbatim — including relative href/src/data values —
  // not the browser-resolved absolute URL a live element's .href/.src
  // property would give you. Left relative, those links/embeds break once
  // the extracted content is served from ReadOne's own origin instead of
  // the source page's. img[src] is also handled here directly (in addition
  // to normalizeImages's more involved lazy-load/stashed-attr handling)
  // since a plain, non-lazy <img> can reach this function without ever
  // going through that logic — resolveUrl() is idempotent on an
  // already-absolute URL, so re-resolving it here is harmless.
  function resolveRelativeURLs(root) {
    root.querySelectorAll('a[href]').forEach(a => {
      const href = a.getAttribute('href');
      if (href && !/^(https?:|mailto:|tel:|#|javascript:)/i.test(href)) {
        a.setAttribute('href', resolveUrl(href));
      }
    });

    root.querySelectorAll('object[data]').forEach(obj => {
      const data = obj.getAttribute('data');
      if (data && !data.startsWith('data:')) obj.setAttribute('data', resolveUrl(data));
    });

    root.querySelectorAll('img, video, audio, source').forEach(el => {
      for (const attr of ['src', 'poster']) {
        const val = el.getAttribute(attr);
        if (val && !val.startsWith('data:')) el.setAttribute(attr, resolveUrl(val));
      }
    });

    root.querySelectorAll('img[srcset], source[srcset]').forEach(el => {
      const srcset = el.getAttribute('srcset');
      if (!srcset) return;
      el.setAttribute(
        'srcset',
        srcset.split(',').map(part => {
          const [u, size] = part.trim().split(/\s+/);
          return size ? `${resolveUrl(u)} ${size}` : resolveUrl(u);
        }).join(', ')
      );
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

  // Some pages (notably archive.today snapshots) lay out an article's body
  // as several sibling <div>s interleaved with empty/display:none ones
  // (originally ad slots or embeds, stripped down but left in place). Each
  // real content div scores independently in Readability's _grabArticle(),
  // and the interleaving breaks its sibling-merge heuristic enough that
  // only the highest-scoring chunk gets kept — the rest of the article is
  // silently dropped. Collapsing each such run of siblings into one node
  // before Readability ever sees them avoids the fragmentation entirely.
  // Applied recursively (top-down, re-descending into the merged node)
  // since the same pattern can repeat at nested depths.
  function mergeFragmentedContent(root) {
    function process(el) {
      const children = Array.from(el.children);
      if (children.length >= 3) {
        const lens = children.map(c => c.textContent.trim().length);
        const substantial = children.filter((c, i) => lens[i] > 200);
        const trivial = children.filter((c, i) => lens[i] <= 50);
        if (substantial.length >= 2 && substantial.length + trivial.length === children.length) {
          const target = substantial[0];
          for (const c of children) {
            if (c === target) continue;
            while (c.firstChild) target.appendChild(c.firstChild);
            c.remove();
          }
        }
      }
      Array.from(el.children).forEach(process);
    }
    process(root);
  }

  // Readability's own boilerplate cleanup can strip an <li>'s children
  // without removing the <li> itself (e.g. a "Recommended Reading" list
  // reduced to `<li></li><li></li>`), or strip every <li> out of a list
  // without removing the now-empty <ul>/<ol> wrapper. Both just render as
  // stray blank space. Runs bottom-up so emptiness cascades correctly (an
  // <li> containing only an empty nested list is itself empty).
  function stripEmptyLists(html) {
    const doc = new DOMParser().parseFromString(html, 'text/html');
    const mediaSelector = 'img, picture, video, audio, iframe, svg';
    function hasMeaningfulContent(el) {
      return el.textContent.trim() !== '' || !!el.querySelector(mediaSelector);
    }
    function process(el) {
      Array.from(el.children).forEach(process);
      if (el.tagName === 'LI' && !hasMeaningfulContent(el)) {
        el.remove();
      } else if ((el.tagName === 'UL' || el.tagName === 'OL') && !el.querySelector(':scope > li')) {
        el.remove();
      }
    }
    process(doc.body);
    return doc.body.innerHTML;
  }

  async function extract() {
    await ensureLibraries();
    await autoScrollFullPage();
    promoteNoscriptImages(document.body);
    normalizeImages(document.body);
    resolveRelativeURLs(document.body);
    convertBackgroundImages(document.body);

    const lead = findLeadImage(document.body);

    const docClone = document.cloneNode(true);
    mergeFragmentedContent(docClone.body);
    const reader = new unsafeWindow.Readability(docClone, { charThreshold: 100 });
    const art = reader.parse();
    if (art) art.content = stripEmptyLists(art.content);
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
    const td = new unsafeWindow.TurndownService({
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

  const TRIGGER_WIDTH = 40;
  const TRIGGER_PEEK = 14; // px visible while tucked away
  const TRIGGER_TOP_KEY = 're-trigger-top';

  // Same book-with-heart mark as static/assets/logo.svg, inlined so the
  // trigger button doesn't depend on ReadOne's server being reachable (it
  // has to render on arbitrary third-party pages) and so its stroke can
  // inherit the button's color via currentColor.
  const TRIGGER_ICON_SVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" width="18" height="18"><path d="M4 19.5v-15A2.5 2.5 0 0 1 6.5 2H19a1 1 0 0 1 1 1v18a1 1 0 0 1-1 1H6.5a1 1 0 0 1 0-5H20" /><path d="M8.62 9.8A2.25 2.25 0 1 1 12 6.836a2.25 2.25 0 1 1 3.38 2.966l-2.626 2.856a.998.998 0 0 1-1.507 0z" /></svg>`;

  function addTrigger() {
    const style = document.createElement('style');
    style.textContent = `
      #re-trigger {
        position: fixed; left: ${TRIGGER_PEEK - TRIGGER_WIDTH}px; z-index: 2147483646;
        width: ${TRIGGER_WIDTH}px; height: ${TRIGGER_WIDTH}px; padding: 0;
        border: none; border-radius: 0 20px 20px 0;
        background: #222; color: #fff; font-size: 18px;
        display: flex; align-items: center; justify-content: center;
        cursor: grab;
        box-shadow: 2px 2px 8px rgba(0,0,0,.3);
        transition: left .15s ease;
        touch-action: none; user-select: none;
      }
      #re-trigger:hover, #re-trigger.re-dragging { left: 0; }
      #re-trigger.re-dragging { cursor: grabbing; transition: none; }
    `;
    document.head.appendChild(style);

    const btn = document.createElement('button');
    btn.id = 're-trigger';
    btn.innerHTML = TRIGGER_ICON_SVG;
    btn.title = 'Open Reader (drag to reposition)';

    const savedTop = typeof GM_getValue === 'function' ? GM_getValue(TRIGGER_TOP_KEY, null) : null;
    btn.style.top = `${savedTop != null ? savedTop : window.innerHeight * 0.5}px`;

    // Drag to reposition vertically, tucked-tab-on-hover to open. A pointer
    // move past a small threshold counts as a drag and suppresses the
    // following click, so dragging doesn't also trigger extraction.
    let dragging = false;
    let moved = false;
    let startY = 0;
    let startTop = 0;

    btn.addEventListener('pointerdown', e => {
      dragging = true;
      moved = false;
      startY = e.clientY;
      startTop = btn.getBoundingClientRect().top;
      btn.setPointerCapture(e.pointerId);
      btn.classList.add('re-dragging');
    });

    btn.addEventListener('pointermove', e => {
      if (!dragging) return;
      const dy = e.clientY - startY;
      if (Math.abs(dy) > 4) moved = true;
      const top = Math.max(0, Math.min(startTop + dy, window.innerHeight - TRIGGER_WIDTH));
      btn.style.top = `${top}px`;
    });

    const endDrag = () => {
      if (!dragging) return;
      dragging = false;
      btn.classList.remove('re-dragging');
      if (moved && typeof GM_setValue === 'function') {
        GM_setValue(TRIGGER_TOP_KEY, parseFloat(btn.style.top));
      }
    };
    btn.addEventListener('pointerup', endDrag);
    btn.addEventListener('pointercancel', endDrag);

    btn.addEventListener('click', async () => {
      if (moved) { moved = false; return; }
      btn.disabled = true;
      btn.textContent = '⏳';
      try {
        const art = await extract();
        if (!art) {
          alert('Readability could not parse this page.');
          return;
        }
        renderReaderView(art);
      } finally {
        btn.disabled = false;
        btn.innerHTML = TRIGGER_ICON_SVG;
      }
    });

    document.body.appendChild(btn);
  }

  // Don't show the Reader button on ReadOne's own pages (the article list,
  // reader view, etc.) — there's nothing to extract there, and clicking it
  // would just try to Readability-parse the app's own UI.
  if (!saveConfigured || location.origin !== SAVE_URL) {
    addTrigger();
  }
})();
