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
  // A SKIP MUST NOT READ AS A PASS WHERE NOBODY IS WATCHING.
  //
  // This exited 0 unconditionally, which is right on a contributor's laptop
  // and wrong in CI: a runner without Chrome would report the browser suite
  // green while running none of it. That is structurally the same defect as
  // the `tsc --noEmit` gate that checked nothing and exited 0 (2026-08-06) —
  // a check incapable of failing is not a check.
  //
  // So the two cases are distinguished by intent rather than guessed at.
  // REQUIRE_BROWSER_TESTS=1 in CI makes absent Chrome a hard failure; locally
  // it stays an explicit, loud skip. CI sets it, so forgetting to install
  // Chrome there breaks the build rather than passing quietly.
  const required = process.env.REQUIRE_BROWSER_TESTS === '1';
  const lines = [
    `browser harness ${required ? 'FAILED' : 'SKIPPED'}: no Chrome found.`,
    '  Tried: CHROME_PATH, google-chrome, google-chrome-stable, chromium, chromium-browser.',
    '  Install Chrome or Chromium, or set CHROME_PATH, then re-run `npm run test:browser`.',
  ];
  if (required) {
    lines.push('  REQUIRE_BROWSER_TESTS=1 is set, so this is an error rather than a skip.');
    console.error(lines.join('\n'));
    process.exit(1);
  }
  console.log(lines.join('\n'));
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
