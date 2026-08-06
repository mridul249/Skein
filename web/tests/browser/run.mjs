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
import { createServer } from 'node:net';
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

// A PORT NOBODY ELSE HOLDS, asked of the kernel rather than guessed.
//
// This used to be `5200 + random(300)` with --strictPort, which is a race
// against anything else on the machine: Vite EXITS IMMEDIATELY when the port
// is taken, and the harness did not watch for that. It polled a dead port
// instead, and eventually printed "vite did not start" with no reason - which
// is exactly the false negative seen in CI. Binding port 0 and reading back
// what the kernel assigned removes the guess entirely.
function freePort() {
  return new Promise((resolve, reject) => {
    const srv = createServer();
    srv.unref();
    srv.on('error', reject);
    srv.listen(0, '127.0.0.1', () => {
      const { port } = srv.address();
      // Closed before Vite binds it. A brief window remains in which another
      // process could take it, but it is far narrower than a random guess,
      // and viteExited below now turns that into a clear error rather than a
      // 20-second silence.
      srv.close(() => resolve(port));
    });
  });
}

const port = await freePort();
const vite = spawn('npx', ['vite', '--port', String(port), '--strictPort'], {
  cwd: webRoot,
  stdio: ['ignore', 'pipe', 'pipe'],
});

let viteLog = '';
vite.stdout.on('data', (d) => { viteLog += d; });
vite.stderr.on('data', (d) => { viteLog += d; });

// WATCH THE PROCESS, NOT THE LOG. Vite's stdout format is not an API and
// changes between versions; whether the port answers is the actual question.
// The exit watcher is what makes a real failure - crash, missing dependency,
// port collision - fail fast and say why, instead of timing out silently.
let viteExited = null;
vite.on('exit', (c, sig) => { viteExited = { code: c, signal: sig }; });
vite.on('error', (e) => { viteExited = { code: null, signal: null, error: e.message }; });

// READINESS IS A PROBE, and it asks for the fixture the tests actually load
// rather than for `/`: Vite answers the root before the TypeScript pipeline is
// warm, so a root probe can succeed while the first real request still fails.
async function waitForVite(timeoutMs = 60_000) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    if (viteExited) return { ok: false, reason: describeExit(viteExited) };
    try {
      const res = await fetch(`http://127.0.0.1:${port}/tests/browser/fixture.html`);
      if (res.ok) return { ok: true };
    } catch {
      /* not listening yet */
    }
    await sleep(250);
  }
  return { ok: false, reason: `it did not answer on port ${port} within ${timeoutMs / 1000}s` };
}

function describeExit({ code, signal, error }) {
  if (error) return `it could not be started: ${error}`;
  if (signal) return `it was killed by ${signal}`;
  return `it exited with code ${code}`;
}

let code = 1;
try {
  const ready = await waitForVite();
  if (!ready.ok) {
    console.error(
      `browser harness FAILED: vite did not start - ${ready.reason}.\n` +
        (viteLog.trim() || '(vite produced no output)'),
    );
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
