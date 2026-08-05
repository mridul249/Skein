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

test('every glyph the interface draws comes from a vendored face', async () => {
  await on({}, async (p) => {
    await p.goto(url('/'));
    const missing = await p.eval(`(async () => {
      await document.fonts.ready;
      const loaded = [...document.fonts].map((f) => f.family + ' ' + f.weight + ' ' + f.status);
      // A codepoint outside a subset falls back to another face at another
      // advance width. For the data face that breaks column alignment, so it
      // is measured rather than assumed — document.fonts.check() returns true
      // for codepoints a face does not contain and cannot detect this.
      const cv = document.createElement('canvas').getContext('2d');
      cv.font = '32px "JetBrains Mono"';
      const cell = cv.measureText('M').width;
      const wanted = [...'0123456789.,%/-\u2014\u00b7\u2713\u2715\u203a'];
      const offGrid = wanted
        .filter((ch) => Math.abs(cv.measureText(ch).width - cell) > 0.01)
        .map((ch) => 'U+' + ch.codePointAt(0).toString(16).toUpperCase());
      return { loaded, offGrid };
    })()`);
    assert.deepEqual(missing.offGrid, [], 'these codepoints fell back to another font');
    // Only the weights actually vendored may be declared: anything else gets
    // synthesised by the browser, which is a different and worse letterform.
    assert.deepEqual(
      missing.loaded.sort(),
      ['IBM Plex Sans 400 loaded', 'IBM Plex Sans 600 loaded', 'JetBrains Mono 400 loaded'],
    );
  });
});

test('prose is sans, data is mono, and no weight is synthesised', async () => {
  await on({}, async (p) => {
    await p.goto(url('/'));
    const r = await p.eval(`(() => {
      const fam = (el) => getComputedStyle(el).fontFamily;
      const body = fam(document.body);
      // Anything marked as data must resolve to the mono face.
      const data = [...document.querySelectorAll('.tabular')].map(fam);
      // No element may ask for a weight that is not vendored (400 / 600).
      const weights = new Set();
      for (const el of document.querySelectorAll('body *')) {
        if (el.textContent && !el.children.length) weights.add(getComputedStyle(el).fontWeight);
      }
      return { body, data: [...new Set(data)], weights: [...weights].sort() };
    })()`);
    assert.match(r.body, /IBM Plex Sans/, 'prose is the sans face');
    for (const f of r.data) assert.match(f, /JetBrains Mono/, 'data is the mono face');
    assert.ok(r.data.length > 0, 'the fixture should render some data');
    assert.deepEqual(r.weights, ['400', '600'], 'only vendored weights may be used');
  });
});

test('nothing scrolls horizontally at 375px, including the file list', async () => {
  for (const route of ['/', '/trash', '/settings', '/login']) {
    await on({ width: 375, height: 800 }, async (p) => {
      await p.goto(url(route));
      const r = await p.eval(`(() => {
        const de = document.documentElement;
        de.scrollLeft = 9999;
        const moved = de.scrollLeft;
        de.scrollLeft = 0;
        // A horizontally scrolling table is not an acceptable phone layout:
        // the columns it pushes off are Size, Stored and Added, i.e. every
        // column except the one you already knew. Below md the table is
        // replaced by a stacked list, so nothing may scroll sideways.
        // Only elements that can actually be scrolled count: sr-only and
        // truncate both overflow by design and clip with overflow hidden,
        // which is not a scroller.
        const scrollers = [...document.querySelectorAll('*')]
          .filter((e) => {
            if (e.scrollWidth <= e.clientWidth + 1) return false;
            const ox = getComputedStyle(e).overflowX;
            return ox === 'auto' || ox === 'scroll';
          })
          .map((e) => e.tagName + '.' + String(e.className).slice(0, 40));
        return { moved, scrollers };
      })()`);
      assert.equal(r.moved, 0, `${route} must not scroll sideways`);
      assert.deepEqual(r.scrollers, [], `${route} has a horizontally scrolling element`);
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
    assert.equal(segs[2].bg, 'rgb(28, 32, 39)', 'the last block is the free remainder');
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
      const btn = [...document.querySelectorAll('[role="img"][aria-label*="shard"]')][0];
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
    assert.equal(chips[2].bg, 'rgb(60, 67, 80)', 'an unidentifiable shard is a neutral grey');
  });
});

test('every shard carries its account number, and an unknown one says so', async () => {
  await on({}, async (p) => {
    await p.goto(url('/'));
    const labels = await p.eval(`(() => {
      const btn = [...document.querySelectorAll('[role="img"][aria-label*="shard"]')][0];
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

test('a transfer bar never claims completion the server has not reported', async () => {
  await on({}, async (p) => {
    await p.goto(url('/') + '&transfers=1');
    const r = await p.eval(`(() => {
      const rows = [...document.querySelectorAll('[aria-label="Transfers"] li')];
      return rows.map((li) => {
        const track = li.querySelector('.rounded-full.bg-raised');
        const fill = track?.firstElementChild;
        const t = track?.getBoundingClientRect();
        const f = fill?.getBoundingClientRect();
        return {
          label: li.querySelector('span:last-of-type')?.textContent?.trim() ?? '',
          fillFraction: t && f ? f.width / t.width : null,
        };
      });
    })()`);
    const finishing = r.find((x) => /Finishing/.test(x.label));
    assert.ok(finishing, 'the fixture should include a job in the finishing state');
    // Every byte has left the machine, but the server is still writing shards.
    // A full bar here is the lie this whole state exists to avoid.
    assert.ok(
      finishing.fillFraction < 0.9,
      `finishing must not render a full bar (was ${finishing.fillFraction})`,
    );
  });
});

test('the shard map tiles the file exactly, in index order', async () => {
  await on({}, async (p) => {
    await p.goto(url('/'));
    const r = await p.eval(`(async () => {
      // Select the striped file.
      const rows = [...document.querySelectorAll('tbody tr')];
      const striped = rows.find((tr) => /archive.tar.zst/.test(tr.textContent));
      striped.querySelector('button').click();
      await new Promise((r) => setTimeout(r, 350));

      const pane = document.querySelector('aside[aria-label^="Details for"]');
      if (!pane) return { error: 'detail pane did not open' };
      const map = pane.querySelector('[role="img"]');
      const track = map.getBoundingClientRect();
      const segs = [...map.children].map((el) => el.getBoundingClientRect());

      // Gaps between consecutive segments: a hairline is 1px of background
      // showing through, anything wider is a gutter and reads as pieces.
      const gaps = [];
      for (let i = 1; i < segs.length; i++) gaps.push(segs[i].left - segs[i - 1].right);

      return {
        order: [...pane.querySelectorAll('ul li')].map((li) =>
          (li.textContent.match(/#(\\d+)/) || [])[1],
        ),
        covered: segs.reduce((a, b) => a + b.width, 0) + gaps.reduce((a, b) => a + b, 0),
        trackWidth: track.width,
        maxGap: gaps.length ? Math.max(...gaps) : 0,
        segCount: segs.length,
      };
    })()`);
    assert.ok(!r.error, r.error);
    assert.equal(r.segCount, 3, 'one segment per shard');
    // Tiles exactly: segments plus their hairlines cover the whole track.
    assert.ok(
      Math.abs(r.covered - r.trackWidth) < 1.5,
      `segments must tile the track (covered ${r.covered} of ${r.trackWidth})`,
    );
    assert.ok(r.maxGap <= 1.5, `boundaries must be hairlines, not gutters (max ${r.maxGap}px)`);
    assert.deepEqual(r.order, ['0', '1', '2'], 'shards are listed in index order');

    // Opening the pane narrows the listing column. The table must absorb that
    // rather than start scrolling sideways underneath it.
    const scrollers = await p.eval(`(() => [...document.querySelectorAll('*')]
      .filter((e) => {
        if (e.scrollWidth <= e.clientWidth + 1) return false;
        const ox = getComputedStyle(e).overflowX;
        return ox === 'auto' || ox === 'scroll';
      })
      .map((e) => e.tagName + '.' + String(e.className).slice(0, 40)))()`);
    assert.deepEqual(scrollers, [], 'the detail pane must not force the listing to scroll');
  });
});

test('no shard claims to be verified, because nothing has verified it', async () => {
  await on({}, async (p) => {
    await p.goto(url('/'));
    const r = await p.eval(`(async () => {
      const rows = [...document.querySelectorAll('tbody tr')];
      rows.find((tr) => /archive.tar.zst/.test(tr.textContent)).querySelector('button').click();
      await new Promise((r) => setTimeout(r, 350));
      const pane = document.querySelector('aside[aria-label^="Details for"]');
      const states = [...pane.querySelectorAll('ul li .sr-only')].map((e) => e.textContent.trim());
      const striped = { states, text: pane.textContent };

      // Now a file whose shards all resolve, to check the other summary.
      document.querySelector('aside[aria-label^="Details for"] button[aria-label="Close details"]').click();
      await new Promise((r) => setTimeout(r, 200));
      const rows2 = [...document.querySelectorAll('tbody tr')];
      rows2.find((tr) => /truncation-behaviour/.test(tr.textContent)).querySelector('button').click();
      await new Promise((r) => setTimeout(r, 350));
      const pane2 = document.querySelector('aside[aria-label^="Details for"]');

      return { striped, intact: { states: [...pane2.querySelectorAll('ul li .sr-only')].map((e) => e.textContent.trim()), text: pane2.textContent } };
    })()`);
    // The API reports no integrity field at all, so "Verified" would be a
    // claim nothing supports.
    assert.deepEqual(r.striped.states, ['Unverified', 'Unverified', 'Missing']);
    assert.match(r.striped.text, /1 of 3 shards unreachable/);
    assert.deepEqual(r.intact.states, ['Unverified']);
    assert.match(r.intact.text, /none verified yet/);
    assert.ok(!/\bVerified\b/.test(r.intact.text), 'nothing may read as verified');
  });
});

test('an image preview renders, and a media element issues a Range request', async () => {
  await on({}, async (p) => {
    // Watch every request the page makes, so the Range claim is read off the
    // wire rather than inferred from the markup.
    const ranged = [];
    await p.send('Network.enable');
    p.on('Network.requestWillBeSent', (ev) => {
      const h = ev.request.headers || {};
      const range = h.Range ?? h.range;
      if (range) ranged.push({ url: ev.request.url, range });
    });

    await p.goto(url('/') + '&preview=1');

    // The image: a real 64x64 PNG, so a decoded width proves it loaded.
    const img = await p.eval(`(async () => {
      const rows = [...document.querySelectorAll('tbody tr')];
      rows.find((tr) => /photo.png/.test(tr.textContent)).querySelector('button').click();
      await new Promise((r) => setTimeout(r, 600));
      const el = document.querySelector('aside[aria-label^="Details for"] img');
      return el ? { src: el.getAttribute('src'), w: el.naturalWidth, h: el.naturalHeight } : null;
    })()`);
    assert.ok(img, 'an image file must render an <img> preview');
    assert.equal(img.w, 64, 'the image actually decoded');
    assert.equal(img.h, 64);

    // The video: recorded as it appears rather than queried afterwards. The
    // fixture's clip.mp4 is deliberately not decodable — it only has to be
    // large enough for a media element to fetch it — so the element removes
    // itself once onError fires, and a later query would race that.
    const vid = await p.eval(`(async () => {
      window.__seen = null;
      const obs = new MutationObserver(() => {
        const el = document.querySelector('aside[aria-label^="Details for"] video');
        if (el && !window.__seen) {
          window.__seen = {
            preload: el.getAttribute('preload'),
            autoplay: el.autoplay,
            controls: el.controls,
            src: el.getAttribute('src'),
          };
        }
      });
      obs.observe(document.body, { childList: true, subtree: true });

      document.querySelector('aside[aria-label^="Details for"] button[aria-label="Close details"]').click();
      await new Promise((r) => setTimeout(r, 200));
      const rows = [...document.querySelectorAll('tbody tr')];
      rows.find((tr) => /clip.mp4/.test(tr.textContent)).querySelector('button').click();
      await new Promise((r) => setTimeout(r, 1500));
      obs.disconnect();

      const fallback = document.querySelector('aside[aria-label^="Details for"]').textContent;
      return { seen: window.__seen, fallback };
    })()`);
    assert.ok(vid.seen, 'a video file must render a <video> preview');
    // An undecodable file must degrade to a clear instruction, not a blank box.
    assert.match(vid.fallback, /cannot be played here/);
    assert.equal(vid.seen.preload, 'metadata', 'must not preload the whole file');
    assert.equal(vid.seen.autoplay, false, 'a file manager never autoplays');
    assert.equal(vid.seen.controls, true);

    // Known issue #16: the server's ranged read has been correct since Gate 0
    // but no element in the UI could reach it. This is that element.
    const clip = ranged.filter((r) => /clip\.mp4/.test(r.url));
    assert.ok(
      clip.length > 0,
      `the media element must issue a Range request; saw ${JSON.stringify(ranged)}`,
    );
  });
});

test('nothing in the drawer is clipped by an ancestor', async () => {
  await on({}, async (p) => {
    await p.goto(url('/'));
    const r = await p.eval(`(async () => {
      const rows = [...document.querySelectorAll('tbody tr')];
      rows.find((tr) => /archive.tar.zst/.test(tr.textContent)).querySelector('button').click();
      await new Promise((r) => setTimeout(r, 450));

      const drawer = document.querySelector('aside[aria-label^="Details for"]');
      if (!drawer) return { error: 'drawer did not open' };
      const box = drawer.getBoundingClientRect();

      // Known issue #27 in its new shape. The old popover was clipped by the
      // file card's overflow; a fixed drawer has no ancestor that can clip it,
      // and this walks the chain to prove that rather than assume it.
      const clippers = [];
      for (let el = drawer.parentElement; el && el !== document.documentElement; el = el.parentElement) {
        const st = getComputedStyle(el);
        if (st.overflowX !== 'visible' || st.overflowY !== 'visible') {
          const b = el.getBoundingClientRect();
          if (box.bottom > b.bottom + 1 || box.right > b.right + 1 ||
              box.top < b.top - 1 || box.left < b.left - 1) {
            clippers.push(el.tagName + '.' + String(el.className).slice(0, 40));
          }
        }
      }

      // The shard map inside it must be fully visible too, not cut off by the
      // drawer's own scroll box.
      const map = drawer.querySelector('[role="img"][aria-label*="shards in order"]');
      const m = map.getBoundingClientRect();
      const de = document.documentElement;

      return {
        position: getComputedStyle(drawer).position,
        clippers,
        drawerInViewport: box.top >= -1 && box.right <= de.clientWidth + 1,
        mapVisible: m.width > 0 && m.left >= box.left - 1 && m.right <= box.right + 1,
      };
    })()`);
    assert.ok(!r.error, r.error);
    assert.equal(r.position, 'fixed', 'the drawer is positioned outside the listing');
    assert.deepEqual(r.clippers, [], 'no ancestor may clip the drawer');
    assert.ok(r.drawerInViewport, 'the drawer must sit inside the viewport');
    assert.ok(r.mapVisible, 'the shard map must be fully inside the drawer');
  });
});

test('the quota tooltip is still never clipped by an ancestor', async () => {
  await on({}, async (p) => {
    await p.goto(url('/'));
    // The overlay primitive survives the drawer, for the quota bar. Its
    // clipping guarantee is the original #27 fix and is still load-bearing.
    // Driven with a real pointer rather than a programmatic .focus(): a
    // headless page has no system focus, so el.focus() moves activeElement
    // without dispatching the focus event React listens for. Verified —
    // activeElement became the button and no tooltip appeared.
    const at = JSON.parse(
      await p.eval(`(() => {
        const b = document.querySelector('[role="group"][aria-label^="Pooled storage"] button');
        const r = b.getBoundingClientRect();
        return JSON.stringify({ x: r.x + r.width / 2, y: r.y + r.height / 2 });
      })()`),
    );
    await p.send('Input.dispatchMouseEvent', { type: 'mouseMoved', x: at.x, y: at.y, buttons: 0 });
    await sleep(400);

    const r = await p.eval(`(async () => {
      const panel = document.querySelector('[role="tooltip"]');
      if (!panel) return { error: 'tooltip did not open on hover' };
      const root = panel.closest('[data-overlay-root]');
      const box = panel.getBoundingClientRect();
      const de = document.documentElement;
      return {
        parent: root?.parentElement?.tagName,
        inViewport: box.top >= -1 && box.left >= -1 &&
                    box.bottom <= de.clientHeight + 1 && box.right <= de.clientWidth + 1,
      };
    })()`);
    assert.ok(!r.error, r.error);
    assert.equal(r.parent, 'BODY', 'the tooltip is portalled out of the listing');
    assert.ok(r.inViewport, 'the tooltip must sit inside the viewport');
  });
});

test('opening the drawer does not shift the row that opened it', async () => {
  await on({}, async (p) => {
    await p.goto(url('/'));
    // Re-aimed from the popover. The failure it guards is unchanged: opening a
    // panel must not reflow the thing that opened it out from under the
    // pointer. The old panel did it via a scrollbar; a drawer would do it by
    // giving the listing a right padding, which is why it overlays instead.
    const r = await p.eval(`(async () => {
      const rows = [...document.querySelectorAll('tbody tr')];
      const row = rows.find((tr) => /archive.tar.zst/.test(tr.textContent));
      const card = row.closest('.card');
      const before = row.getBoundingClientRect();
      const beforeCard = { w: card.clientWidth, sh: card.scrollHeight };
      row.querySelector('button').click();
      await new Promise((r) => setTimeout(r, 450));
      const after = row.getBoundingClientRect();
      return {
        dx: Math.round(after.x - before.x),
        dy: Math.round(after.y - before.y),
        dw: Math.round(after.width - before.width),
        cardW: card.clientWidth - beforeCard.w,
        cardSh: card.scrollHeight - beforeCard.sh,
      };
    })()`);
    assert.equal(r.dx, 0, 'the row must not move horizontally');
    assert.equal(r.dy, 0, 'the row must not move vertically');
    assert.equal(r.dw, 0, 'the row must not be resized');
    assert.equal(r.cardW, 0, 'the listing must not gain or lose width');
    assert.equal(r.cardSh, 0, "the drawer must not add to the listing's scroll height");
  });
});

test('clicking a second row swaps the drawer contents without reopening', async () => {
  await on({}, async (p) => {
    await p.goto(url('/'));
    // Replaces the hover-bridge test, which has no drawer equivalent: a drawer
    // opens on click, so there is no gap between trigger and panel to cross.
    // The equivalent risk is tearing the panel down and rebuilding it on every
    // selection, which would throw away scroll position and focus.
    const r = await p.eval(`(async () => {
      const pick = async (name) => {
        const rows = [...document.querySelectorAll('tbody tr')];
        rows.find((tr) => tr.textContent.includes(name)).querySelector('button').click();
        await new Promise((r) => setTimeout(r, 400));
      };

      await pick('archive.tar.zst');
      const first = document.querySelector('aside[aria-label^="Details for"]');
      first.dataset.probe = 'same-node';
      const firstLabel = first.getAttribute('aria-label');

      await pick('truncation-behaviour');
      const second = document.querySelector('aside[aria-label^="Details for"]');
      return {
        stayedMounted: second?.dataset.probe === 'same-node',
        firstLabel,
        secondLabel: second?.getAttribute('aria-label'),
      };
    })()`);
    assert.ok(r.stayedMounted, 'the drawer must swap contents, not remount');
    assert.match(r.firstLabel, /archive\.tar\.zst/);
    assert.match(r.secondLabel, /truncation-behaviour/);
    assert.notEqual(r.firstLabel, r.secondLabel, 'the contents must actually change');
  });
});

test('the drawer does not trap focus and does not dim the page', async () => {
  await on({}, async (p) => {
    await p.goto(url('/'));
    const r = await p.eval(`(async () => {
      const rows = [...document.querySelectorAll('tbody tr')];
      rows.find((tr) => /archive.tar.zst/.test(tr.textContent)).querySelector('button').click();
      await new Promise((r) => setTimeout(r, 450));

      const drawer = document.querySelector('aside[aria-label^="Details for"]');
      return {
        // A drawer is a second pane, not an interruption. Both of these would
        // make picking a different file impossible, which is the main thing
        // someone does with it open.
        ariaModal: drawer.getAttribute('aria-modal'),
        role: drawer.getAttribute('role'),
        // No dimming backdrop anywhere over the listing.
        backdrops: [...document.querySelectorAll('div')].filter((el) => {
          const st = getComputedStyle(el);
          if (st.position !== 'fixed') return false;
          const b = el.getBoundingClientRect();
          const covers = b.width >= window.innerWidth - 1 && b.height >= window.innerHeight - 1;
          return covers && st.backgroundColor !== 'rgba(0, 0, 0, 0)';
        }).length,
        // The listing behind it is still clickable and still hit-testable.
        rowStillOnTop: (() => {
          const row = rows.find((tr) => /notes|truncation/.test(tr.textContent))
            ?? rows[rows.length - 1];
          const b = row.getBoundingClientRect();
          const hit = document.elementFromPoint(b.left + 40, b.top + b.height / 2);
          return row.contains(hit);
        })(),
      };
    })()`);
    assert.notEqual(r.ariaModal, 'true', 'the drawer must not be aria-modal');
    assert.notEqual(r.role, 'dialog', 'the drawer is not a dialog');
    assert.equal(r.backdrops, 0, 'the drawer must not dim the page');
    assert.ok(r.rowStillOnTop, 'the listing must stay clickable behind the drawer');

    // Tab must be able to leave the drawer. The modal traps on purpose; this
    // must not, or a keyboard user cannot get back to the file list.
    const escaped = await p.eval(`document.activeElement.closest('aside[aria-label^="Details for"]') !== null`);
    assert.ok(escaped, 'focus should start inside the drawer');
    let left = false;
    for (let i = 0; i < 30 && !left; i++) {
      await p.tab();
      left = await p.eval(
        `document.activeElement.closest('aside[aria-label^="Details for"]') === null`,
      );
    }
    assert.ok(left, 'tabbing must be able to leave the drawer');
  });
});

test('Escape closes the drawer and returns focus to the row that opened it', async () => {
  await on({}, async (p) => {
    await p.goto(url('/'));
    await p.eval(`(async () => {
      const rows = [...document.querySelectorAll('tbody tr')];
      rows.find((tr) => /archive.tar.zst/.test(tr.textContent)).querySelector('button').click();
      await new Promise((r) => setTimeout(r, 450));
    })()`);
    assert.equal(
      await p.eval(`!!document.querySelector('aside[aria-label^="Details for"]')`),
      true,
    );

    await p.press('Escape', 'Escape', 27);
    await sleep(400);

    const r = await p.eval(`(() => {
      const row = [...document.querySelectorAll('tbody tr')]
        .find((tr) => /archive.tar.zst/.test(tr.textContent));
      return {
        closed: !document.querySelector('aside[aria-label^="Details for"]'),
        focusedTrigger: document.activeElement === row.querySelector('button'),
      };
    })()`);
    assert.ok(r.closed, 'Escape must close the drawer');
    assert.ok(r.focusedTrigger, 'focus must return to the row that opened it');
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

// THE UPLOAD RUN-THROUGH, driven through the real store and real components.
//
// Two concurrent uploads, navigate away and back, both survive and neither is
// dismissible; finish them; dismiss one; clear the rest.
test('uploads survive navigation, and only terminal cards can be dismissed', async () => {
  await on({}, async (p) => {
    await p.goto(url('/') + '&transfers=1');

    const readRows = `(() => [...document.querySelectorAll('[aria-label="Transfers"] li')].map((li) => ({
      name: li.querySelector('span')?.textContent?.trim() ?? '',
      action: li.querySelector('button')?.textContent?.trim() ?? '',
    })))()`;

    const before = await p.eval(readRows);
    assert.equal(before.length, 2, 'the fixture should seed two concurrent uploads');
    // Mid-flight, the only control is Cancel. A Dismiss here would orphan a
    // running upload, which is issue #13.
    for (const row of before) {
      assert.equal(row.action, 'Cancel', `${row.name} offered "${row.action}" mid-flight`);
    }

    // Navigate away and back. The store lives above the router, so the jobs
    // must still be there.
    await p.eval(`(() => {
      const trash = [...document.querySelectorAll('a')].find((a) => /Trash/i.test(a.textContent ?? ''));
      trash?.click();
      return true;
    })()`);
    await sleep(150);
    await p.eval(`(() => {
      const files = [...document.querySelectorAll('a')].find((a) => /Files/i.test(a.textContent ?? ''));
      files?.click();
      return true;
    })()`);
    await sleep(150);

    const after = await p.eval(readRows);
    assert.equal(after.length, 2, 'navigating away lost an in-flight upload (issue #13)');
    for (const row of after) {
      assert.equal(row.action, 'Cancel', 'an in-flight upload became dismissible');
    }

    // There is nothing to clear while both are running.
    const clearBefore = await p.eval(
      `Boolean(document.querySelector('[aria-label^="Clear"]'))`,
    );
    assert.equal(clearBefore, false, 'Clear appeared with no finished transfers');

    // Finish both.
    await p.eval(`window.__settleUpload('finishing-archive', 'done')`);
    await p.eval(`window.__settleUpload('sending-video', 'error')`);
    await sleep(200);

    const settled = await p.eval(readRows);
    assert.equal(settled.length, 2);
    for (const row of settled) {
      assert.equal(row.action, 'Dismiss', `${row.name} is terminal but not dismissible`);
    }

    // Dismiss one.
    await p.eval(`(() => {
      const btn = [...document.querySelectorAll('[aria-label="Transfers"] li button')]
        .find((b) => b.textContent.trim() === 'Dismiss');
      btn?.click();
      return true;
    })()`);
    await sleep(150);
    assert.equal((await p.eval(readRows)).length, 1, 'Dismiss did not remove the card');

    // Clear the rest.
    await p.eval(`(() => {
      document.querySelector('[aria-label^="Clear"]')?.click();
      return true;
    })()`);
    await sleep(150);
    const panel = await p.eval(`Boolean(document.querySelector('[aria-label="Transfers"]'))`);
    assert.equal(panel, false, 'Clear left the transfers panel behind');
  });
});

// Clear must leave in-flight uploads running, not cancel them.
test('Clear finished leaves an in-flight upload alone', async () => {
  await on({}, async (p) => {
    await p.goto(url('/') + '&transfers=1');

    // Finish exactly one of the two.
    await p.eval(`window.__settleUpload('finishing-archive', 'done')`);
    await sleep(200);

    await p.eval(`(() => {
      document.querySelector('[aria-label^="Clear"]')?.click();
      return true;
    })()`);
    await sleep(150);

    const rows = await p.eval(`(() => [...document.querySelectorAll('[aria-label="Transfers"] li')].map((li) => ({
      name: li.querySelector('span')?.textContent?.trim() ?? '',
      action: li.querySelector('button')?.textContent?.trim() ?? '',
    })))()`);

    assert.equal(rows.length, 1, 'Clear removed an in-flight upload');
    assert.match(rows[0].name, /sending-video/);
    assert.equal(rows[0].action, 'Cancel', 'the surviving upload is no longer in flight');
  });
});

// The download list must not imply tracking it cannot do.
test('the download list shows no progress affordance', async () => {
  await on({}, async (p) => {
    await p.goto(url('/'));
    const r = await p.eval(`(() => {
      const panel = document.querySelector('[aria-label="Downloads"]');
      if (!panel) return { present: false };
      return {
        present: true,
        text: panel.textContent ?? '',
        bars: panel.querySelectorAll('[role="progressbar"], .animate-sweep').length,
      };
    })()`);
    if (!r.present) return; // no downloads seeded on this route
    assert.equal(r.bars, 0, 'the download panel renders a progress affordance it cannot back');
    assert.ok(!/%/.test(r.text), 'the download panel shows a percentage it cannot know');
  });
});

// THE SELECTION RUN-THROUGH. Select rows, delete with a forced partial
// failure, and confirm the result is rendered honestly.
test('selection drives a toolbar, and the header checkbox is tri-state', async () => {
  await on({}, async (p) => {
    await p.goto(url('/'));

    const state = `(() => {
      const boxes = [...document.querySelectorAll('tbody input[type=checkbox]')];
      const head = document.querySelector('thead input[type=checkbox]');
      const bar = document.querySelector('[aria-label="Selection actions"]');
      return {
        rows: boxes.length,
        headChecked: head?.checked ?? null,
        headIndeterminate: head?.indeterminate ?? null,
        toolbar: bar ? (bar.textContent ?? '').replace(/\\s+/g, ' ').trim() : null,
      };
    })()`;

    const initial = await p.eval(state);
    assert.ok(initial.rows >= 2, 'the fixture needs at least two file rows');
    assert.equal(initial.toolbar, null, 'the toolbar showed with nothing selected');
    assert.equal(initial.headIndeterminate, false);

    // Select one row: the header goes indeterminate, not checked.
    await p.eval(`(() => {
      document.querySelectorAll('tbody input[type=checkbox]')[0].click();
      return true;
    })()`);
    await sleep(120);

    const one = await p.eval(state);
    assert.equal(one.headIndeterminate, true, 'header is not indeterminate with a partial selection');
    assert.equal(one.headChecked, false, 'header claims all rows are selected');
    assert.ok(/1 selected/.test(one.toolbar ?? ''), `toolbar read "${one.toolbar}"`);

    // Select all from the header.
    await p.eval(`(() => { document.querySelector('thead input[type=checkbox]').click(); return true; })()`);
    await sleep(120);

    const all = await p.eval(state);
    assert.equal(all.headChecked, true);
    assert.equal(all.headIndeterminate, false);
    assert.ok(
      new RegExp(`${all.rows} selected`).test(all.toolbar ?? ''),
      `toolbar read "${all.toolbar}" for ${all.rows} rows`,
    );
  });
});

test('a partial bulk failure is reported honestly and keeps failures selected', async () => {
  await on({}, async (p) => {
    // One of the fixture's files will fail.
    await p.goto(url('/') + '&bulkfail=1');

    await p.eval(`(() => { document.querySelector('thead input[type=checkbox]').click(); return true; })()`);
    await sleep(120);

    const total = await p.eval(`document.querySelectorAll('tbody input[type=checkbox]').length`);

    await p.eval(`(() => {
      // Matches both labels: the file list trashes ("Move to trash"), the
      // trash view purges ("Delete permanently").
      const btn = [...document.querySelectorAll('[aria-label="Selection actions"] button')]
        .find((b) => /trash|Delete/i.test(b.textContent ?? ''));
      btn?.click();
      return true;
    })()`);
    await sleep(400);

    const r = await p.eval(`(() => {
      const notice = document.querySelector('[role="status"]');
      const bar = document.querySelector('[aria-label="Selection actions"]');
      return {
        text: notice ? (notice.textContent ?? '').replace(/\\s+/g, ' ').trim() : null,
        toolbar: bar ? (bar.textContent ?? '').replace(/\\s+/g, ' ').trim() : null,
      };
    })()`);

    assert.ok(r.text, 'no outcome was rendered after a partial failure');
    // Neither a bare success nor a bare error: both counts appear.
    assert.ok(
      new RegExp(`${total - 1} of ${total}`).test(r.text),
      `the notice must name both counts, read "${r.text}"`,
    );
    assert.ok(/1 failed/.test(r.text), `the notice must name the failures, read "${r.text}"`);
    assert.ok(
      /rate limiting/.test(r.text),
      `the notice must say WHY it failed, read "${r.text}"`,
    );
    assert.ok(/Retry 1 failed/.test(r.text), 'no retry affordance for the failures');

    // The failed file stays selected so Retry is one click.
    assert.ok(
      /1 selected/.test(r.toolbar ?? ''),
      `the failed file should stay selected, toolbar read "${r.toolbar}"`,
    );
  });
});

// The checkbox has to belong to the theme. The native control paints an
// opaque white box on a dark page, which is what appearance-none removes.
test('checkboxes are themed, filled on select, and carry no glyph', async () => {
  await on({}, async (p) => {
    await p.goto(url('/'));

    const read = `(() => {
      const box = document.querySelector('tbody input[type=checkbox]');
      const cs = getComputedStyle(box);
      const before = getComputedStyle(box, '::before');
      const after = getComputedStyle(box, '::after');
      return {
        appearance: cs.appearance,
        background: cs.backgroundColor,
        border: cs.borderTopColor,
        glyphs: [before.content, after.content].filter((c) => c && c !== 'none' && c !== 'normal' && c !== '""'),
      };
    })()`;

    const unchecked = await p.eval(read);
    assert.equal(unchecked.appearance, 'none', 'the native control still paints itself');

    // Unchecked sits on the page background, not white.
    const rgb = unchecked.background.match(/\d+/g).map(Number);
    assert.ok(
      rgb[0] < 60 && rgb[1] < 60 && rgb[2] < 60,
      `unchecked background is ${unchecked.background}, expected the dark canvas`,
    );

    await p.eval(`(() => { document.querySelector('tbody input[type=checkbox]').click(); return true; })()`);
    await sleep(120);

    const checked = await p.eval(read);
    const on = checked.background.match(/\d+/g).map(Number);
    // The accent is #c48af0 — red and blue high, and distinctly not the
    // unchecked background.
    assert.ok(
      on[0] > 150 && on[2] > 180,
      `checked background is ${checked.background}, expected the purple accent`,
    );
    assert.equal(checked.glyphs.length, 0, `a glyph was drawn inside: ${checked.glyphs}`);
  });
});

// The download rows read as columns, not as ragged text. Regression for the
// flex/justify-between layout that put the status wherever the name ended.
test('download rows align their status and action columns', async () => {
  await on({}, async (p) => {
    await p.goto(url('/') + '&downloads=1');

    const r = await p.eval(`(() => {
      const rows = [...document.querySelectorAll('[aria-label="Downloads"] li')];
      if (rows.length < 2) return null;
      return rows.map((li) => {
        const spans = li.querySelectorAll('span');
        const status = spans[spans.length - 1];
        const btn = li.querySelector('button');
        return {
          statusLeft: Math.round(status.getBoundingClientRect().left),
          actionRight: Math.round(btn.getBoundingClientRect().right),
        };
      });
    })()`);
    if (!r) return; // fixture seeds fewer than two downloads on this route

    const lefts = new Set(r.map((x) => x.statusLeft));
    const rights = new Set(r.map((x) => x.actionRight));
    assert.equal(lefts.size, 1, `status column is ragged across rows: ${[...lefts]}`);
    assert.equal(rights.size, 1, `action column is ragged across rows: ${[...rights]}`);
  });
});

// THE CHECK THAT WOULD HAVE CAUGHT THE DEAD DRAWER.
//
// Every piece of the Go-side download path was built, unit-tested and verified
// live through a headless harness, and the feature was still invisible in the
// running app: the capability probe went out with no Authorization header, the
// server answered 401, and the probe correctly read that as "not the desktop
// build". Both binary-level route tests passed the whole time, because the
// routes really were in the binary — nothing asserted that the UI reached them.
//
// So this drives the real chain: fetch -> probe -> `desktop` -> which component
// Layout mounts. The fixture's stub returns 401 for an unauthenticated probe,
// exactly as the real server does, so an unauthenticated client fails this.
test('the desktop build renders the desktop drawer, not the browser list', async () => {
  await on({}, async (p) => {
    await p.goto(url('/') + '&desktop=1');

    const which = await p.eval(
      `(async () => {
        // The probe is async; wait for it to settle rather than sampling once.
        for (let i = 0; i < 50; i++) {
          const el = document.querySelector('[data-download-ui]');
          if (el && el.dataset.downloadUi === 'desktop') return 'desktop';
          await new Promise((r) => setTimeout(r, 40));
        }
        const el = document.querySelector('[data-download-ui]');
        return el ? el.dataset.downloadUi : 'absent';
      })()`,
    );

    assert.equal(
      which,
      'desktop',
      'the desktop build is still rendering the browser download list; the ' +
        'capability probe never reported the desktop path',
    );
  });
});

// The other direction, so the test above cannot pass by the swap being stuck on.
test('without the capability the browser list is what renders', async () => {
  await on({}, async (p) => {
    await p.goto(url('/'));

    const which = await p.eval(`(() => {
      const el = document.querySelector('[data-download-ui]');
      return el ? el.dataset.downloadUi : 'absent';
    })()`);

    assert.equal(which, 'browser', 'the browser build rendered the desktop drawer');
  });
});

// THE SEQUENCE THAT ACTUALLY SHIPPED BROKEN.
//
// The provider mounts above the router, so the capability probe fires BEFORE
// login. From the running app's log:
//
//   16:47:24.419  GET /api/desktop/capabilities  status=401
//   16:47:33.024  POST /api/auth/login           status=200
//
// The probe failed closed on a request that could not succeed, cached that,
// and every download for the rest of the session took the browser path. The
// test above did not catch it because the fixture installed a session before
// mount — so the probe never saw the unauthenticated state. This one does not.
test('the drawer appears when the session arrives after the probe', async () => {
  await on({}, async (p) => {
    await p.goto(url('/') + '&desktop=1&latelogin=1');

    const which = await p.eval(`(async () => {
      for (let i = 0; i < 80; i++) {
        const el = document.querySelector('[data-download-ui]');
        if (el && el.dataset.downloadUi === 'desktop') return 'desktop';
        await new Promise((r) => setTimeout(r, 40));
      }
      const el = document.querySelector('[data-download-ui]');
      return el ? el.dataset.downloadUi : 'absent';
    })()`);

    assert.equal(
      which,
      'desktop',
      'the desktop path stayed disabled after login: the pre-auth 401 was cached, ' +
        'or nothing re-probed when the session appeared',
    );
  });
});
