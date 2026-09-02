// gawk project site — the two bits of behaviour the landing page has:
// the join-card "typing" demo in the hero, and the copy buttons on the
// quickstart / helm snippets. No framework, no build step.
(() => {
  'use strict';

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
