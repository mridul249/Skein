/**
 * Runner for the browser harness.
 *
 * Starts a Vite dev server on an ephemeral port, runs the browser tests
 * against it, then shuts the server down. Deliberately a separate npm script
 * from `npm test`: `node --test` stays the fast path for logic like
 * uploads.ts, and this must never gate a build.
 *
 * With no Chrome installed it prints why and exits 0. A machine without a
 * browser should not fail a suite it cannot run; it should say so.
 */
import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { dirname, resolve } from 'node:path';

import { findChrome, sleep } from './chrome.mjs';

const here = dirname(fileURLToPath(import.meta.url));
const webRoot = resolve(here, '../..');

const chrome = findChrome();
if (!chrome) {
  console.log('browser harness SKIPPED: no Chrome found.');
  console.log('  Tried: CHROME_PATH, google-chrome, google-chrome-stable, chromium, chromium-browser.');
  console.log('  Install Chrome or Chromium, or set CHROME_PATH, then re-run `npm run test:browser`.');
  process.exit(0);
}

const port = 5200 + Math.floor(Math.random() * 300);
const vite = spawn('npx', ['vite', '--port', String(port), '--strictPort'], {
  cwd: webRoot,
  stdio: ['ignore', 'pipe', 'pipe'],
});

let viteLog = '';
vite.stdout.on('data', (d) => { viteLog += d; });
vite.stderr.on('data', (d) => { viteLog += d; });

async function waitForVite() {
  for (let i = 0; i < 80; i++) {
    await sleep(250);
    try {
      const res = await fetch(`http://127.0.0.1:${port}/tests/browser/fixture.html`);
      if (res.ok) return true;
    } catch {
      /* not up yet */
    }
  }
  return false;
}

let code = 1;
try {
  if (!(await waitForVite())) {
    console.error('browser harness FAILED: vite did not start.\n' + viteLog);
    process.exit(1);
  }

  const child = spawn(
    process.execPath,
    ['--test', 'tests/browser/ui.test.mjs'],
    {
      cwd: webRoot,
      stdio: 'inherit',
      env: { ...process.env, SKEIN_FIXTURE: `http://127.0.0.1:${port}/tests/browser/fixture.html`, CHROME_BIN: chrome },
    },
  );
  code = await new Promise((r) => child.on('exit', r));
} finally {
  vite.kill('SIGTERM');
  await sleep(300);
}

process.exit(code ?? 1);
