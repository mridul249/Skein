/**
 * A minimal Chrome DevTools Protocol driver.
 *
 * Why a real browser rather than jsdom: every defect this harness exists to
 * catch is a *layout* defect — a tab order, a computed style, a box that
 * overflows its parent. jsdom computes no layout, so it would report all of
 * them as passing while costing three dependencies. This file has none: it
 * speaks CDP over the WebSocket built into Node 22 and drives whatever Chrome
 * is already installed.
 *
 * Assert on computed values — `getComputedStyle`, `getBoundingClientRect`,
 * `document.activeElement` — never on markup shape. Asserting the DOM tree
 * would buy jsdom's weaknesses at Chrome's price.
 */
import { spawn, spawnSync } from 'node:child_process';
import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

const CANDIDATES = [
  process.env.CHROME_PATH,
  'google-chrome',
  'google-chrome-stable',
  'chromium',
  'chromium-browser',
].filter(Boolean);

/** Returns a runnable Chrome command, or null when none is installed. */
export function findChrome() {
  for (const bin of CANDIDATES) {
    const probe = spawnSync(bin, ['--version'], { stdio: 'ignore' });
    if (probe.status === 0) return bin;
  }
  return null;
}

/**
 * Runs `fn` against a headless Chrome page, then tears everything down.
 * `width`/`height` set a real CSS viewport via device emulation — note that
 * Chrome clamps `--window-size` to about 500px, so a 375px check that does not
 * use emulation is silently measuring 500px instead.
 */
export async function withChrome({ bin, width = 1280, height = 900, mobile = false }, fn) {
  const port = 9400 + Math.floor(Math.random() * 500);
  const profile = mkdtempSync(join(tmpdir(), 'skein-cdp-'));
  const chrome = spawn(
    bin,
    [
      '--headless=new',
      '--disable-gpu',
      '--no-sandbox',
      // Deliberately NOT --hide-scrollbars. That flag makes scrollbars take
      // no width, which silently hid a real defect: opening a panel inside a
      // scroll container raised a scrollbar, narrowed the content box by 10px
      // and shifted the trigger out from under the pointer. Headless must
      // lay out the way the user's browser does, or the harness certifies
      // bugs as fixed.
      '--no-first-run',
      '--disable-extensions',
      `--remote-debugging-port=${port}`,
      `--user-data-dir=${profile}`,
      'about:blank',
    ],
    { stdio: 'ignore' },
  );

  let target;
  for (let i = 0; i < 80; i++) {
    await sleep(250);
    try {
      const list = await (await fetch(`http://127.0.0.1:${port}/json/list`)).json();
      target = list.find((t) => t.type === 'page');
      if (target) break;
    } catch {
      /* not listening yet */
    }
  }
  if (!target) {
    chrome.kill('SIGTERM');
    rmSync(profile, { recursive: true, force: true });
    throw new Error('Chrome started but never exposed a debugging target');
  }

  const ws = new WebSocket(target.webSocketDebuggerUrl);
  await new Promise((resolve, reject) => {
    ws.addEventListener('open', resolve, { once: true });
    ws.addEventListener('error', reject, { once: true });
  });

  let seq = 0;
  const pending = new Map();
  // CDP events, as opposed to command replies. Subscribed to by `page.on`,
  // which is how a test can read what actually went over the wire — request
  // headers, for instance — rather than inferring it from the DOM.
  const handlers = new Map();
  ws.addEventListener('message', (ev) => {
    const msg = JSON.parse(ev.data);
    if (!msg.id) {
      for (const fn of handlers.get(msg.method) ?? []) fn(msg.params);
      return;
    }
    if (!pending.has(msg.id)) return;
    const { resolve, reject } = pending.get(msg.id);
    pending.delete(msg.id);
    if (msg.error) reject(new Error(JSON.stringify(msg.error)));
    else resolve(msg.result);
  });

  const send = (method, params = {}) =>
    new Promise((resolve, reject) => {
      const id = ++seq;
      pending.set(id, { resolve, reject });
      ws.send(JSON.stringify({ id, method, params }));
    });

  const page = {
    send,
    /** Subscribes to a CDP event, e.g. 'Network.requestWillBeSent'. */
    on(method, fn) {
      if (!handlers.has(method)) handlers.set(method, []);
      handlers.get(method).push(fn);
    },
    async goto(url) {
      await send('Page.enable');
      await send('Emulation.setDeviceMetricsOverride', {
        width,
        height,
        deviceScaleFactor: 1,
        mobile,
      });
      await send('Page.navigate', { url });
      await sleep(2500);
    },
    /** Evaluates in the page and returns the value. Throws page exceptions. */
    async eval(expression) {
      const r = await send('Runtime.evaluate', {
        expression,
        awaitPromise: true,
        returnByValue: true,
      });
      if (r.exceptionDetails) {
        throw new Error(r.exceptionDetails.exception?.description ?? 'page threw');
      }
      return r.result.value;
    },
    async key(type, key, code, keyCode, extra = {}) {
      return send('Input.dispatchKeyEvent', {
        type,
        key,
        code,
        windowsVirtualKeyCode: keyCode,
        nativeVirtualKeyCode: keyCode,
        ...extra,
      });
    },
    /** Enter and Space need `text` on a real keyDown, or Chrome never
     *  synthesises the click that activates a focused <button>. */
    async press(key, code, keyCode) {
      const text = key === 'Enter' ? '\r' : key === ' ' ? ' ' : undefined;
      await this.key(text ? 'keyDown' : 'rawKeyDown', key, code, keyCode, text ? { text } : {});
      await this.key('keyUp', key, code, keyCode);
      await sleep(250);
    },
    async tab(shift = false) {
      const modifiers = shift ? 8 : 0;
      await this.key('rawKeyDown', 'Tab', 'Tab', 9, { modifiers });
      await this.key('keyUp', 'Tab', 'Tab', 9, { modifiers });
      await sleep(120);
    },
    async type(text) {
      for (const ch of text) {
        await send('Input.dispatchKeyEvent', {
          type: 'keyDown',
          text: ch,
          unmodifiedText: ch,
          key: ch,
        });
        await send('Input.dispatchKeyEvent', { type: 'keyUp', key: ch });
      }
      await sleep(150);
    },
  };

  try {
    return await fn(page);
  } finally {
    ws.close();
    chrome.kill('SIGTERM');
    await sleep(300);
    rmSync(profile, { recursive: true, force: true });
  }
}

export const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
