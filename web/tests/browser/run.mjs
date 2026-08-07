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

// FAIL ON THE CAUSE, NOT ON A SYMPTOM 30 SECONDS LATER.
//
// chrome.mjs speaks CDP over the WebSocket client built into Node, and carries
// no `ws` dependency by design. That global arrived in Node 22. On Node 20 it
// is undefined, and every test dies with
//
//   ReferenceError: WebSocket is not defined
//
// once per test - 31 identical stacks pointing at chrome.mjs, none of which
// says "your Node is too old". CI was pinned to 20 and hit exactly this the
// moment the Vite address bug below stopped masking it. `engines` in
// package.json is advisory (npm only warns), so the check is enforced here.
if (typeof WebSocket === 'undefined') {
  console.error(
    `browser harness FAILED: this Node (${process.version}) has no global WebSocket.\n` +
      '  The CDP driver needs Node 22 or newer. Upgrade Node, or run `npm test`\n' +
      '  for the logic suite, which has no such requirement.',
  );
  process.exit(1);
}

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

// THE ONE ADDRESS the server binds, the port is reserved on, and every probe
// asks for. A single constant rather than three literals, because the bug this
// file exists to have fixed was exactly those three drifting apart — see the
// note at the spawn below.
const HOST = '127.0.0.1';

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
    srv.listen(0, HOST, () => {
      const { port } = srv.address();
      // Closed before Vite binds it. A brief window remains in which another
      // process could take it, but it is far narrower than a random guess,
      // and viteExited below now turns that into a clear error rather than a
      // 20-second silence.
      srv.close(() => resolve(port));
    });
  });
}

// BIND THE ADDRESS WE PROBE, rather than letting the two be decided separately.
//
// THE BUG THIS FIXES, because it cost a green CI run and reads as a flake:
// with no --host, Vite binds whatever `localhost` RESOLVES to. On GitHub's
// ubuntu-latest (and inside node:20/node:22 containers) /etc/hosts maps
// localhost to BOTH ::1 and 127.0.0.1, and Node's default `verbatim` DNS order
// returns ::1 first — so Vite listens on ::1 ONLY. /proc/net/tcp is empty and
// /proc/net/tcp6 holds the single listener. The harness then polled the
// literal 127.0.0.1, got ECONNREFUSED for the full 60s, and printed
//
//   browser harness FAILED: vite did not start - it did not answer on
//   port 44137 within 60s.
//   VITE v6.4.3 ready in 182 ms
//
// which is self-contradictory on its face: Vite HAD started, on an address
// nobody was asking. Reproduced 2026-08-07 by reading the kernel listener
// tables under both Node 20 and 22; the Node version is NOT the variable, the
// resolution of `localhost` is. It passed on the author's machine only because
// that /etc/hosts maps localhost to 127.0.0.1 alone.
//
// Passing --host pins the bind explicitly, so HOST is the one source of truth
// for both the server and every probe below. 127.0.0.1 is the choice because
// Chrome must reach it too, and CDP's own endpoint is IPv4.
const port = await freePort();
const vite = spawn('npx', ['vite', '--port', String(port), '--strictPort', '--host', HOST], {
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
      // PER-REQUEST TIMEOUT, and it is load-bearing rather than tidiness.
      //
      // fetch() has NO default timeout. Against a socket that accepts a
      // connection and never answers - something else holding the port, a
      // half-dead server, a proxy - it hangs forever, so this loop never gets
      // back to the viteExited check or to its own deadline. Measured: still
      // hanging after 30s with no timeout set. The whole readiness check
      // becomes an indefinite block, which is worse than the bug it replaced.
      const res = await fetch(`http://${HOST}:${port}/tests/browser/fixture.html`, {
        signal: AbortSignal.timeout(2_000),
      });
      if (res.ok) return { ok: true };
    } catch {
      /* not listening yet, or it did not answer in time */
    }
    await sleep(250);
  }
  // NAME THE ADDRESS, not just the port. The old text said "vite did not
  // start", which was flatly untrue in the failure that prompted this: Vite
  // had started and said so in the log printed directly underneath, on a
  // different address. An error that contradicts the evidence beside it sends
  // the reader looking for a startup fault that does not exist.
  return {
    ok: false,
    reason:
      `nothing answered http://${HOST}:${port} within ${timeoutMs / 1000}s. ` +
      `If vite reported "ready" below, it bound a DIFFERENT address than the ` +
      `one probed - check that --host ${HOST} was passed`,
  };
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
      `browser harness FAILED: ${ready.reason}.\n` +
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
      env: { ...process.env, SKEIN_FIXTURE: `http://${HOST}:${port}/tests/browser/fixture.html`, CHROME_BIN: chrome },
    },
  );
  code = await new Promise((r) => child.on('exit', r));
} finally {
  vite.kill('SIGTERM');
  await sleep(300);
}

process.exit(code ?? 1);
