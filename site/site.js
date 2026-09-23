// gawk project site — the three bits of behaviour the landing page has:
// the join-card "typing" demo in the hero, the copy buttons on the
// quickstart / helm snippets, and the download cards, which fill themselves
// in from the per-component release manifests. No framework, no build step.
(() => {
  'use strict';

  // --- Download cards (R46, docs/46): <div class="dl" data-dl="<component>">.
  //
  // The newest release of each native app is read from
  // releases/<component>/latest.json on the repository's `badges` branch,
  // written by the CI job that attaches the binaries. A static file over raw
  // GitHub rather than the releases API: no per-visitor rate limit, and
  // `releases/latest` would name whichever of the six components tagged
  // last. The markup ships with a working fallback (the button links to the
  // releases page), so a fetch that fails, or a manifest that has never been
  // published, leaves a usable card rather than a broken one.
  const manifestUrl = (component) =>
    `https://raw.githubusercontent.com/Tuhis/gawk/badges/releases/${component}/latest.json`;
  const fmtSize = (bytes) => `${(bytes / 1048576).toFixed(bytes >= 10485760 ? 0 : 1)} MB`;
  const fmtDate = (iso) => {
    const d = new Date(iso);
    return isNaN(d) ? '' : d.toLocaleDateString(undefined, { year: 'numeric', month: 'short', day: 'numeric' });
  };
  // Only what the card displays is trusted from the manifest, and only in
  // the shape the writer's validator guarantees; anything else leaves the
  // static fallback in place.
  const usable = (m) =>
    m && m.schema === 1 && typeof m.version === 'string' && /^\d+\.\d+\.\d+$/.test(m.version) &&
    m.asset && typeof m.asset.url === 'string' && m.asset.url.startsWith('https://github.com/Tuhis/gawk/releases/download/') &&
    typeof m.asset.sha256 === 'string' && /^[0-9a-f]{64}$/.test(m.asset.sha256) &&
    typeof m.release_url === 'string' && m.release_url.startsWith('https://github.com/Tuhis/gawk/releases/tag/');

  document.querySelectorAll('[data-dl]').forEach((card) => {
    const component = card.dataset.dl;
    fetch(manifestUrl(component), { cache: 'no-cache' })
      .then((r) => (r.ok ? r.json() : null))
      .then((m) => {
        if (!usable(m)) return;
        const link = card.querySelector('[data-dl-link]');
        const ver = card.querySelector('[data-dl-version]');
        const sum = card.querySelector('[data-dl-sum]');
        const sha = card.querySelector('[data-dl-sha]');
        link.href = m.asset.url;
        link.removeAttribute('target');
        link.setAttribute('download', '');
        ver.textContent = '';
        const notes = document.createElement('a');
        notes.href = m.release_url;
        notes.target = '_blank';
        notes.rel = 'noopener';
        notes.textContent = `v${m.version}`;
        ver.appendChild(notes);
        const when = fmtDate(m.published_at);
        const size = Number.isFinite(m.asset.size) ? fmtSize(m.asset.size) : '';
        ver.appendChild(document.createTextNode([when, size].filter(Boolean).map((s) => ` · ${s}`).join('')));
        sha.textContent = m.asset.sha256;
        sum.hidden = false;
        // A distribution with no release yet (docs/54: the Mac app before its
        // first signed release) stays hidden until its manifest is real.
        if (card.hasAttribute('data-dl-until-released')) card.hidden = false;
      })
      .catch(() => {});
  });

  // --- Hero: type a demo code into the join card, then clear and repeat.
  const code = document.querySelector('[data-demo-code]');
  if (code) {
    const boxes = Array.from(code.querySelectorAll('.code-box'));
    const ring = code.querySelector('.code-ring');
    const demo = (code.dataset.demoCode || '5UP4XW').toUpperCase().slice(0, boxes.length);
    const reduced = window.matchMedia('(prefers-reduced-motion: reduce)').matches;

    const render = (typed) => {
      boxes.forEach((b, i) => { b.textContent = typed[i] || ''; });
      const active = Math.min(typed.length, boxes.length - 1);
      if (ring) ring.style.left = (active * 52) + 'px';
    };

    if (reduced) {
      render(demo);
    } else {
      let typed = '';
      const tick = () => {
        if (typed.length >= demo.length) {
          setTimeout(() => { typed = ''; render(typed); setTimeout(tick, 700); }, 2600);
        } else {
          typed = demo.slice(0, typed.length + 1);
          render(typed);
          setTimeout(tick, 430);
        }
      };
      render('');
      setTimeout(tick, 900);
    }
  }

  // --- Copy buttons: <button class="copy" data-copy-target="#id">.
  document.querySelectorAll('button[data-copy-target]').forEach((btn) => {
    const target = document.querySelector(btn.dataset.copyTarget);
    if (!target) return;
    const label = btn.textContent;
    let timer = 0;
    const done = () => {
      btn.textContent = 'Copied';
      clearTimeout(timer);
      timer = setTimeout(() => { btn.textContent = label; }, 1600);
    };
    btn.addEventListener('click', () => {
      const text = target.innerText.replace(/^\$ /gm, '');
      if (navigator.clipboard) navigator.clipboard.writeText(text).then(done, () => {});
      else done();
    });
  });
})();
