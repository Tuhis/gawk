// R23 (docs/29 §4.2). An operator can replace the terms body with their own
// HTML fragment served from `config.termsUrl`. That fragment is fetched at
// runtime and rendered inside the SPA origin, so it MUST be sanitized before
// it reaches the DOM — an unsanitized operator document is a stored-XSS vector
// against every visitor. This is an allowlist sanitizer (deny by default):
// only known-inert elements/attributes survive; everything else is dropped or
// unwrapped. Whitelisting beats blacklisting for URL schemes and attributes,
// which is why href is checked against an allowed-scheme list rather than a
// "block javascript:" pattern.
//
// Parsing uses DOMParser('text/html'), which produces an INERT document:
// scripts do not execute and resources (img/iframe) do not load while we walk
// it. The output is a string re-serialized from the scrubbed tree, intended
// for a single dangerouslySetInnerHTML — the DOMPurify pattern.

// Block/inline elements a plain long-form legal document needs. No media, no
// interactive controls, no metadata, no embedding.
const ALLOWED_TAGS = new Set([
  'H1', 'H2', 'H3', 'H4', 'H5', 'H6',
  'P', 'UL', 'OL', 'LI', 'DL', 'DT', 'DD',
  'STRONG', 'EM', 'B', 'I', 'U', 'SMALL', 'SUP', 'SUB',
  'A', 'BR', 'HR', 'BLOCKQUOTE', 'CODE', 'PRE',
  'SECTION', 'ARTICLE', 'HEADER', 'FOOTER', 'DIV', 'SPAN',
  'TABLE', 'THEAD', 'TBODY', 'TR', 'TH', 'TD', 'CAPTION',
]);

// Per-tag attribute allowlist. Everything not listed here is stripped — that
// removes every on* handler, style, class, id, srcset, formaction, etc. in one
// move, because none of them are ever added.
const ALLOWED_ATTRS: Record<string, Set<string>> = {
  A: new Set(['href']),
};

// Elements whose *content* is also unsafe or meaningless as text — removed
// whole, not unwrapped. (An unwrapped <style> would dump its CSS as visible
// text; an unwrapped <svg> could still smuggle a <script>/foreignObject.)
const DROP_WITH_CONTENT = new Set([
  'SCRIPT', 'STYLE', 'IFRAME', 'OBJECT', 'EMBED', 'APPLET',
  'LINK', 'META', 'BASE', 'TITLE', 'HEAD', 'NOSCRIPT', 'TEMPLATE',
  'SVG', 'MATH', 'FORM', 'INPUT', 'BUTTON', 'TEXTAREA', 'SELECT', 'OPTION',
  'AUDIO', 'VIDEO', 'SOURCE', 'CANVAS', 'IMG', 'PICTURE',
]);

// href schemes we permit. Note: no `data:` (SVG/HTML data URLs are an XSS
// vector) and no scheme-relative surprises — a bare `#anchor` or path is fine.
const SAFE_HREF = /^(https?:|mailto:|#|\/|\.\/|\.\.\/)/i;

/**
 * Sanitize an operator-supplied terms HTML fragment to a safe HTML string.
 * Returns '' when there is no DOM available (e.g. an SSR context) or the input
 * is empty — callers treat '' as "no override, use the bundled default".
 */
export function sanitizeTermsHtml(html: string): string {
  if (typeof html !== 'string' || html.trim() === '') return '';
  if (typeof DOMParser === 'undefined' || typeof document === 'undefined') return '';

  const doc = new DOMParser().parseFromString(html, 'text/html');
  const body = doc.body;
  if (!body) return '';

  // Collect elements top-down first; mutating the tree while a live
  // NodeList/TreeWalker is positioned in it is a footgun.
  const elements: Element[] = [];
  const walker = doc.createTreeWalker(body, NodeFilter.SHOW_ELEMENT);
  for (let n = walker.nextNode(); n; n = walker.nextNode()) {
    elements.push(n as Element);
  }

  const dropWhole: Element[] = [];
  const unwrap: Element[] = [];

  for (const el of elements) {
    const tag = el.tagName.toUpperCase();

    if (DROP_WITH_CONTENT.has(tag)) {
      dropWhole.push(el);
      continue;
    }
    if (!ALLOWED_TAGS.has(tag)) {
      // Unknown-but-not-dangerous (e.g. <font>, <marquee>): keep the text,
      // drop the wrapper.
      unwrap.push(el);
      continue;
    }

    // Allowed element: scrub every attribute not explicitly permitted.
    const allowed = ALLOWED_ATTRS[tag];
    for (const attr of Array.from(el.attributes)) {
      const name = attr.name.toLowerCase();
      if (!allowed || !allowed.has(name)) {
        el.removeAttribute(attr.name);
        continue;
      }
      if (name === 'href' && !SAFE_HREF.test(attr.value.trim())) {
        el.removeAttribute(attr.name);
      }
    }
    if (tag === 'A' && el.hasAttribute('href')) {
      // Harden any surviving link: no opener, no referrer, opens away from the
      // SPA so it can never navigate the app to an operator-chosen URL.
      el.setAttribute('rel', 'noopener noreferrer nofollow');
      el.setAttribute('target', '_blank');
    }
  }

  // Remove dangerous nodes first (their descendants come with them), then
  // unwrap the rest. Guard on isConnected: a node queued for unwrap may have
  // been inside a node we just removed.
  for (const el of dropWhole) el.remove();
  for (const el of unwrap) {
    if (!el.isConnected) continue;
    const parent = el.parentNode;
    if (!parent) continue;
    while (el.firstChild) parent.insertBefore(el.firstChild, el);
    parent.removeChild(el);
  }

  return body.innerHTML;
}
