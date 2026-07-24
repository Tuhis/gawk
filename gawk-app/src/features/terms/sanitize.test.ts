// @vitest-environment jsdom
// The sanitizer needs a real DOMParser; without a DOM it returns '' by design.
import { describe, expect, it } from 'vitest';
import { sanitizeTermsHtml } from './sanitize';

// The security gate for the R23 operator override (docs/29 §4.2). Every case
// here is an XSS the un-sanitized path would ship. If a mutation flips the
// allowlist to a blocklist (or drops the href scheme check), these fail.

describe('sanitizeTermsHtml — safe content is preserved', () => {
  it('keeps allowed structural + inline elements and their text', () => {
    const out = sanitizeTermsHtml(
      '<h1>Terms</h1><p>Be <strong>lawful</strong> and <em>kind</em>.</p><ul><li>one</li><li>two</li></ul>',
    );
    expect(out).toContain('<h1>Terms</h1>');
    expect(out).toContain('<strong>lawful</strong>');
    expect(out).toContain('<li>two</li>');
  });

  it('keeps http/https/mailto/anchor links and hardens them', () => {
    const out = sanitizeTermsHtml('<a href="https://example.com">site</a>');
    expect(out).toContain('href="https://example.com"');
    expect(out).toContain('rel="noopener noreferrer nofollow"');
    expect(out).toContain('target="_blank"');
    expect(sanitizeTermsHtml('<a href="mailto:x@y.z">mail</a>')).toContain('href="mailto:x@y.z"');
  });

  it('unwraps unknown-but-harmless tags, keeping their text', () => {
    const out = sanitizeTermsHtml('<font color="red">hi</font>');
    expect(out).not.toContain('<font');
    expect(out).not.toContain('color');
    expect(out).toContain('hi');
  });
});

describe('sanitizeTermsHtml — XSS vectors are neutralized', () => {
  it('removes <script> entirely, content and all', () => {
    const out = sanitizeTermsHtml('<p>ok</p><script>alert(1)</script>');
    expect(out).toContain('<p>ok</p>');
    expect(out.toLowerCase()).not.toContain('<script');
    expect(out).not.toContain('alert(1)');
  });

  it('strips inline event handlers from allowed elements', () => {
    const out = sanitizeTermsHtml('<p onclick="alert(1)" onmouseover="x()">hi</p>');
    expect(out).toContain('hi');
    expect(out).not.toContain('onclick');
    expect(out).not.toContain('onmouseover');
  });

  it('drops <img onerror> without keeping the handler', () => {
    const out = sanitizeTermsHtml('<img src="x" onerror="alert(1)">');
    expect(out.toLowerCase()).not.toContain('<img');
    expect(out).not.toContain('onerror');
  });

  it('rejects javascript: hrefs (whitelist, not blacklist)', () => {
    for (const bad of [
      '<a href="javascript:alert(1)">x</a>',
      '<a href="  javascript:alert(1)">x</a>',
      '<a href="JaVaScRiPt:alert(1)">x</a>',
      '<a href="data:text/html,<script>alert(1)</script>">x</a>',
    ]) {
      const out = sanitizeTermsHtml(bad);
      expect(out).not.toContain('javascript:');
      expect(out).not.toContain('data:');
      // the anchor text survives, only the dangerous href is dropped
      expect(out).toContain('x');
    }
  });

  it('removes <style> including its CSS text (not just the tag)', () => {
    const out = sanitizeTermsHtml('<style>body{background:url(javascript:alert(1))}</style><p>ok</p>');
    expect(out.toLowerCase()).not.toContain('<style');
    expect(out).not.toContain('background');
    expect(out).toContain('<p>ok</p>');
  });

  it('removes <iframe>, <object>, <svg>, <form> and friends', () => {
    const out = sanitizeTermsHtml(
      '<iframe src="evil"></iframe><object data="evil"></object><svg><script>alert(1)</script></svg><form action="x"><input></form><p>ok</p>',
    );
    for (const t of ['<iframe', '<object', '<svg', '<script', '<form', '<input']) {
      expect(out.toLowerCase()).not.toContain(t);
    }
    expect(out).toContain('<p>ok</p>');
  });

  it('strips class/style/id from allowed elements (no CSS/selector injection)', () => {
    const out = sanitizeTermsHtml('<div class="x" style="position:fixed" id="y">t</div>');
    expect(out).toContain('t');
    expect(out).not.toContain('class');
    expect(out).not.toContain('style');
    expect(out).not.toContain('id=');
  });
});

describe('sanitizeTermsHtml — degenerate inputs', () => {
  it('returns empty string for empty/whitespace/non-string input', () => {
    expect(sanitizeTermsHtml('')).toBe('');
    expect(sanitizeTermsHtml('   ')).toBe('');
    expect(sanitizeTermsHtml(undefined as unknown as string)).toBe('');
  });
});
