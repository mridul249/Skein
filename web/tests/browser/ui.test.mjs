/**
 * Browser tests. Every assertion here reads a *computed* value — a resolved
 * style, a bounding rect, `document.activeElement` — never the shape of the
 * markup. Asserting the DOM tree would give jsdom's weaknesses at Chrome's
 * cost, and would have caught none of the defects these tests cover.
 */
import test from 'node:test';
import assert from 'node:assert/strict';

import { withChrome, sleep } from './chrome.mjs';

const BASE = process.env.SKEIN_FIXTURE;
const bin = process.env.CHROME_BIN;
const url = (route) => `${BASE}?route=${encodeURIComponent(route)}`;

const on = (opts, fn) => withChrome({ bin, ...opts }, fn);

test('every glyph the character grid needs comes from the vendored subset', async () => {
  await on({}, async (p) => {
    await p.goto(url('/'));
    const { cell, offGrid } = await p.eval(`(async () => {
      await document.fonts.ready;
      const cv = document.createElement('canvas').getContext('2d');
      cv.font = '32px "JetBrains Mono"';
      const cell = cv.measureText('M').width;
      const wanted = [...'\\u203A\\u25B8\\u25BE\\u25CF\\u25CB\\u2713\\u2715\\u00B7\\u232B\\u2500\\u2502\\u2588\\u2593\\u2592\\u2591\\u2190\\u2192\\u2191\\u2193'];
      const offGrid = wanted
        .filter((ch) => Math.abs(cv.measureText(ch).width - cell) > 0.01)
        .map((ch) => 'U+' + ch.codePointAt(0).toString(16).toUpperCase());
      return { cell, offGrid };
    })()`);
    assert.ok(cell > 0, 'the font should have a measurable advance');
    // A codepoint outside the subset falls back to another face at another
    // advance width, which breaks the grid and reads as a layout bug.
    assert.deepEqual(offGrid, [], 'these codepoints fell back to another font');
  });
});

test('no element renders with a corner radius', async () => {
  for (const route of ['/', '/trash', '/settings', '/login']) {
    await on({}, async (p) => {
      await p.goto(url(route));
      const rounded = await p.eval(`(() => {
        const bad = [];
        for (const el of document.querySelectorAll('*')) {
          const s = getComputedStyle(el);
          const r = [s.borderTopLeftRadius, s.borderTopRightRadius,
                     s.borderBottomLeftRadius, s.borderBottomRightRadius];
          if (r.some((v) => v !== '0px')) bad.push(el.tagName + '.' + String(el.className).slice(0, 40));
        }
        return bad;
      })()`);
      assert.deepEqual(rounded, [], `radius 0 everywhere (${route})`);
    });
  }
});

test('the page never scrolls horizontally at 375px', async () => {
  for (const route of ['/', '/trash', '/settings', '/login']) {
    await on({ width: 375, height: 800 }, async (p) => {
      await p.goto(url(route));
      const scrolled = await p.eval(`(() => {
        const de = document.documentElement;
        de.scrollLeft = 9999;
        const moved = de.scrollLeft;
        de.scrollLeft = 0;
        return moved;
      })()`);
      assert.equal(scrolled, 0, `${route} must not scroll sideways`);
    });
  }
});

test('quota bar packs used bytes left, then one remainder', async () => {
  await on({}, async (p) => {
    await p.goto(url('/'));
    const segs = await p.eval(`(() => {
      const bar = document.querySelector('[role="group"][aria-label^="Pooled storage"]');
      return [...bar.children].map((el) => {
        const probe = el.querySelector('button') ?? el;
        return { w: el.getBoundingClientRect().width,
                 bg: getComputedStyle(probe).backgroundColor,
                 label: probe.getAttribute('aria-label') };
      });
    })()`);

    // drive1, drive2, then the grey remainder — no interleaving.
    assert.equal(segs.length, 3, 'two drives plus exactly one remainder');
    assert.equal(segs[0].bg, 'rgb(111, 226, 222)', 'first segment is drive 1');
    assert.equal(segs[1].bg, 'rgb(175, 215, 222)', 'second segment is drive 2');
    assert.equal(segs[2].bg, 'rgb(49, 50, 68)', 'the last block is the free remainder');
    assert.ok(segs[0].w > segs[1].w, '277 GB used should be wider than 8.1 GB used');
    assert.match(segs[0].label, /277 GB of 400 GB used/);
  });
});

test('every quota segment is reachable by keyboard and describes itself', async () => {
  await on({}, async (p) => {
    await p.goto(url('/'));
    // Tab for real rather than calling .focus(): the claim is that a keyboard
    // user can *reach* the segment, and a synthetic FocusEvent would not even
    // drive React, which listens for focusin by delegation.
    let reached = false;
    for (let i = 0; i < 25 && !reached; i++) {
      await p.tab();
      reached = await p.eval(`(() => {
        const bar = document.querySelector('[role="group"][aria-label^="Pooled storage"]');
        return !!bar && bar.contains(document.activeElement)
          && document.activeElement.tagName === 'BUTTON';
      })()`);
    }
    assert.ok(reached, 'tabbing from the top of the page must reach a quota segment');

    await sleep(200);
    const described = await p.eval(`(() => {
      const el = document.activeElement;
      const id = el.getAttribute('aria-describedby');
      if (!id) return { ok: false, why: 'focused element has no aria-describedby' };
      const panel = document.getElementById(id);
      if (!panel) return { ok: false, why: 'aria-describedby points at nothing' };
      return { ok: true, text: panel.textContent };
    })()`);
    assert.ok(described.ok, described.why);
    assert.match(described.text, /free/, 'the description carries the real numbers');
  });
});

test('an orphaned shard never borrows a real drive colour', async () => {
  await on({}, async (p) => {
    await p.goto(url('/'));
    const chips = await p.eval(`(() => {
      const btn = [...document.querySelectorAll('button[aria-label*="shard"]')][0];
      return [...btn.children].map((el) => ({
        bg: getComputedStyle(el).backgroundColor,
        label: el.textContent.trim(),
      }));
    })()`);
    assert.equal(chips.length, 3, 'the striped file has three shards');
    assert.equal(chips[0].bg, 'rgb(111, 226, 222)', 'shard 0 lives on drive 1');
    assert.equal(chips[1].bg, 'rgb(175, 215, 222)', 'shard 1 lives on drive 2');
    // Shard 2 is orphaned. It used to fall back to ordinal 1 and render in
    // drive 1's colour, claiming to live on a drive that does not hold it.
    assert.notEqual(chips[2].bg, chips[0].bg, 'an orphaned shard must not look like drive 1');
    assert.equal(chips[2].bg, 'rgb(69, 71, 90)', 'an unidentifiable shard is neutral surface1');
  });
});

test('every shard carries its account number, and an unknown one says so', async () => {
  await on({}, async (p) => {
    await p.goto(url('/'));
    const labels = await p.eval(`(() => {
      const btn = [...document.querySelectorAll('button[aria-label*="shard"]')][0];
      return [...btn.children].map((el) => el.textContent.trim());
    })()`);
    // Design.md §5: the number is the identity, colour is reinforcement.
    // Measured — no hue-only ramp survives dichromacy (known issue #29).
    assert.deepEqual(labels, ['1', '2', '?'],
      'shards read as an ordered inventory; an unresolvable one is "?", not a number');
  });
});

test('no account colour can be mistaken for a semantic colour', async () => {
  await on({}, async (p) => {
    await p.goto(url('/settings'));
    const { ramp, semantic } = await p.eval(`(() => {
      const probe = (cls) => {
        const el = document.createElement('div');
        el.className = cls; document.body.appendChild(el);
        const c = getComputedStyle(el).backgroundColor; el.remove(); return c;
      };
      return {
        ramp: [1,2,3,4,5,6].map((n) => probe('bg-drive' + n)),
        semantic: ['bg-green','bg-yellow','bg-red'].map(probe),
      };
    })()`);
    assert.equal(new Set(ramp).size, 6, 'six distinct account colours');
    for (const c of ramp) {
      assert.ok(!semantic.includes(c),
        `account colour ${c} is identical to a semantic colour — that shipped (#29)`);
    }
  });
});

test('an overlay panel is never clipped by an ancestor', async () => {
  await on({}, async (p) => {
    await p.goto(url('/'));
    const r = await p.eval(`(async () => {
      const btn = [...document.querySelectorAll('button[aria-label*="shard"]')][0];
      btn.click();
      await new Promise((r) => setTimeout(r, 400));
      const panel = document.querySelector('[role="tooltip"]');
      const box = panel.getBoundingClientRect();

      // Walk every ancestor and check none of them clips the panel. A portal
      // means the chain is body/html only; anything else here would mean the
      // panel had drifted back inside a scroll container.
      const clippers = [];
      for (let el = panel.parentElement; el && el !== document.documentElement; el = el.parentElement) {
        const s = getComputedStyle(el);
        if (s.overflowX !== 'visible' || s.overflowY !== 'visible') {
          const b = el.getBoundingClientRect();
          if (box.bottom > b.bottom + 1 || box.right > b.right + 1 ||
              box.top < b.top - 1 || box.left < b.left - 1) {
            clippers.push(el.tagName + '.' + String(el.className).slice(0, 40));
          }
        }
      }

      const de = document.documentElement;
      const root = panel.closest('[data-overlay-root]');
      return {
        parent: root?.parentElement?.tagName,
        position: getComputedStyle(root).position,
        clippers,
        inViewport: box.top >= -1 && box.left >= -1 &&
                    box.bottom <= de.clientHeight + 1 && box.right <= de.clientWidth + 1,
        box: { t: Math.round(box.top), b: Math.round(box.bottom),
               l: Math.round(box.left), r: Math.round(box.right) },
      };
    })()`);
    assert.equal(r.parent, 'BODY', 'the panel must be portalled to document.body');
    assert.equal(r.position, 'fixed', 'positioned from the viewport, not from an ancestor');
    assert.deepEqual(r.clippers, [], 'no ancestor may clip the panel');
    assert.ok(r.inViewport, `panel must sit inside the viewport: ${JSON.stringify(r.box)}`);
  });
});

test('opening an overlay does not move its own trigger', async () => {
  await on({}, async (p) => {
    await p.goto(url('/'));
    // The old panel was a child of the scrolling card, so opening it grew the
    // card's scrollHeight, raised a scrollbar, narrowed the content box and
    // shifted the trigger 10px left — out from under the cursor that had just
    // opened it. That is the flicker.
    const r = await p.eval(`(async () => {
      const btn = [...document.querySelectorAll('button[aria-label*="shard"]')][0];
      const card = btn.closest('.card');
      const before = { x: btn.getBoundingClientRect().x, w: card.clientWidth, sh: card.scrollHeight };
      btn.click();
      await new Promise((r) => setTimeout(r, 400));
      const after = { x: btn.getBoundingClientRect().x, w: card.clientWidth, sh: card.scrollHeight };
      return { before, after };
    })()`);
    assert.equal(r.after.x, r.before.x, 'the trigger must not move when the panel opens');
    assert.equal(r.after.w, r.before.w, 'no ancestor may gain a scrollbar');
    assert.equal(r.after.sh, r.before.sh, "the panel must not add to an ancestor's scroll height");
  });
});

test('moving the pointer from trigger onto the panel keeps it open', async () => {
  await on({}, async (p) => {
    await p.goto(url('/'));
    const rects = await p.eval(`(async () => {
      const btn = [...document.querySelectorAll('button[aria-label*="shard"]')][0];
      const b = btn.getBoundingClientRect();
      btn.dispatchEvent(new MouseEvent('mouseover', { bubbles: true }));
      btn.dispatchEvent(new MouseEvent('mouseenter', { bubbles: false }));
      return JSON.stringify({ x: b.x + b.width / 2, y: b.y + b.height / 2 });
    })()`);
    const { x, y } = JSON.parse(rects);

    // Real pointer, real gap crossing.
    await p.send('Input.dispatchMouseEvent', { type: 'mouseMoved', x, y, buttons: 0 });
    await sleep(250);
    assert.equal(
      await p.eval(`!!document.querySelector('[role="tooltip"]')`),
      true,
      'hovering the trigger opens the panel',
    );

    const panel = JSON.parse(
      await p.eval(`(() => { const r = document.querySelector('[role="tooltip"]').getBoundingClientRect();
        return JSON.stringify({ x: r.x + r.width / 2, y: r.y + r.height / 2, top: r.top, bottom: r.bottom }); })()`),
    );

    // Step across the gap between the two boxes, then onto the panel itself.
    const from = Math.min(y, panel.bottom);
    const to = Math.max(y, panel.top);
    for (let step = 0; step <= 6; step++) {
      const py = from + ((to - from) * step) / 6;
      await p.send('Input.dispatchMouseEvent', { type: 'mouseMoved', x, y: py, buttons: 0 });
      await sleep(40);
    }
    await p.send('Input.dispatchMouseEvent', { type: 'mouseMoved', x: panel.x, y: panel.y, buttons: 0 });
    await sleep(250);

    assert.equal(
      await p.eval(`!!document.querySelector('[role="tooltip"]')`),
      true,
      'the panel must survive the pointer travelling onto it',
    );
  });
});

test('the modal traps focus, returns it, and cancels on Escape', async () => {
  await on({}, async (p) => {
    await p.goto(url('/trash'));
    await p.eval(`(() => {
      const b = [...document.querySelectorAll('button[aria-label^="Delete"]')][0];
      b.setAttribute('data-trigger', '1');
      b.focus();
      b.click();
    })()`);
    await sleep(400);

    const aria = await p.eval(`(() => {
      const d = document.querySelector('[role="dialog"]');
      const h = document.getElementById(d.getAttribute('aria-labelledby'));
      return { modal: d.getAttribute('aria-modal'), labelled: h ? h.textContent : null };
    })()`);
    assert.equal(aria.modal, 'true');
    assert.match(aria.labelled, /Delete archive\.tar\.zst and its 3 shards\?/);

    // The destructive action must not be the default focus target.
    const first = await p.eval(`document.activeElement.textContent.trim()`);
    assert.equal(first, 'Cancel');

    for (let i = 0; i < 6; i++) await p.tab();
    assert.equal(
      await p.eval(`document.querySelector('[role="dialog"]').contains(document.activeElement)`),
      true,
      'tab must not escape the dialog',
    );
    for (let i = 0; i < 4; i++) await p.tab(true);
    assert.equal(
      await p.eval(`document.querySelector('[role="dialog"]').contains(document.activeElement)`),
      true,
      'shift-tab must not escape the dialog either',
    );

    await p.press('Escape', 'Escape', 27);
    await sleep(300);
    assert.equal(await p.eval(`!document.querySelector('[role="dialog"]')`), true, 'Escape cancels');
    assert.equal(
      await p.eval(`document.activeElement === document.querySelector('[data-trigger="1"]')`),
      true,
      'focus returns to the trigger that opened it',
    );
  });
});

test('a dialog title made of one long token still fits 375px', async () => {
  await on({ width: 375, height: 800 }, async (p) => {
    await p.goto(url('/trash'));
    await p.eval(`[...document.querySelectorAll('button[aria-label^="Delete"]')][1].click()`);
    await sleep(400);
    const fit = await p.eval(`(() => {
      const d = document.querySelector('[role="dialog"]');
      const box = d.getBoundingClientRect();
      const spill = [...d.querySelectorAll('*')]
        .filter((e) => e.getBoundingClientRect().right > box.right + 1).length;
      return { right: box.right, viewport: document.documentElement.clientWidth, spill };
    })()`);
    // `overflow-wrap` wraps the text but does not lower the intrinsic
    // min-content width a flex item's `min-width: auto` floor is computed
    // from; `min-w-0` is what actually keeps the panel inside the viewport.
    assert.ok(fit.right <= fit.viewport + 1, `panel overflowed: ${JSON.stringify(fit)}`);
    assert.equal(fit.spill, 0, 'no content escapes the panel');
  });
});

test('the modal has no entrance animation under prefers-reduced-motion', async () => {
  for (const [value, expected] of [['no-preference', 'modal-in'], ['reduce', 'none']]) {
    await on({}, async (p) => {
      await p.send('Emulation.setEmulatedMedia', {
        features: [{ name: 'prefers-reduced-motion', value }],
      });
      await p.goto(url('/trash'));
      await p.eval(`[...document.querySelectorAll('button[aria-label^="Delete"]')][0].click()`);
      await sleep(300);
      const name = await p.eval(`getComputedStyle(document.querySelector('[role="dialog"]')).animationName`);
      assert.equal(name, expected, `prefers-reduced-motion: ${value}`);
    });
  }
});
