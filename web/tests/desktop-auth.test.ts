/**
 * What the desktop-download client SENDS, not what it does with the reply.
 *
 * Every test in desktop.test.ts injects a fake fetch and asserts on response
 * HANDLING. All eight failure modes passed while the feature was invisible in
 * the running app, because none of them looked at the REQUEST. The probe was
 * asking an authenticated endpoint a question with no credential, getting a
 * 401, and failing closed exactly as designed — on a request that could never
 * have succeeded.
 *
 * So these assert on the outgoing side. Verified against the running desktop
 * app first: /api/desktop/capabilities returns 401 with no header and
 * {"desktop_downloads":true} with one.
 */
import assert from 'node:assert/strict';
import test from 'node:test';

import { probeDesktop, resetDesktopProbe } from '../src/lib/desktop';
import { DesktopDownloadStore, startDesktopDownload } from '../src/lib/desktop-downloads';
import { __setAccessTokenForTests } from '../src/lib/api';

const TOKEN = 'test-access-token';

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

/** Captures the headers of every request the code under test issues. */
function recordingFetch(body: unknown = { desktop_downloads: true, download_dir: '/d' }) {
  const seen: { url: string; headers: Headers }[] = [];
  const impl = (async (input: RequestInfo | URL, init?: RequestInit) => {
    seen.push({ url: String(input), headers: new Headers(init?.headers) });
    return jsonResponse(body);
  }) as unknown as typeof fetch;
  return { seen, impl };
}

// Calls probeDesktop with NO fetch argument, so it exercises the DEFAULT.
// Passing a fake fetch in would bypass the very thing under test: the eight
// existing probe tests all inject one, which is why none of them noticed the
// default was unauthenticated.
test('the capability probe sends the access token', async () => {
  resetDesktopProbe();
  __setAccessTokenForTests(TOKEN);

  const { seen, impl } = recordingFetch();
  const original = globalThis.fetch;
  globalThis.fetch = impl;
  let caps;
  try {
    caps = await probeDesktop();
  } finally {
    globalThis.fetch = original;
  }

  const probeReq = seen[0];
  if (!probeReq) throw new Error('the probe did not issue a request');
  assert.equal(
    probeReq.headers.get('Authorization'),
    `Bearer ${TOKEN}`,
    'the probe sent no Authorization header, so the real endpoint answers 401 ' +
      'and the drawer never appears',
  );
  assert.equal(caps.desktopDownloads, true);
});

test('starting a download sends the access token', async () => {
  __setAccessTokenForTests(TOKEN);
  const { seen, impl } = recordingFetch({ id: 'dl-1', state: 'running' });
  const original = globalThis.fetch;
  globalThis.fetch = impl;
  try {
    await startDesktopDownload('file-1');
  } finally {
    globalThis.fetch = original;
  }

  const startReq = seen[0];
  if (!startReq) throw new Error('no start request was issued');
  assert.equal(
    startReq.headers.get('Authorization'),
    `Bearer ${TOKEN}`,
    'POST /api/desktop/downloads went out unauthenticated',
  );
});

test('resync and cancel send the access token', async () => {
  __setAccessTokenForTests(TOKEN);
  const { seen, impl } = recordingFetch({ downloads: [] });
  const original = globalThis.fetch;
  globalThis.fetch = impl;
  try {
    const store = new DesktopDownloadStore();
    await store.resync();
    await store.cancel('dl-1');
  } finally {
    globalThis.fetch = original;
  }

  assert.equal(seen.length, 2, 'expected a resync and a cancel request');
  for (const req of seen) {
    assert.equal(
      req.headers.get('Authorization'),
      `Bearer ${TOKEN}`,
      `${req.url} went out unauthenticated`,
    );
  }
});

// THE PROBE MUST NOT MEMOIZE A PRE-AUTH FAILURE.
//
// Observed in the running app, from the owner's log:
//
//   16:47:24.419  GET /api/desktop/capabilities  status=401
//   16:47:33.024  POST /api/auth/login           status=200
//
// DownloadsProvider mounts ABOVE the router, so the probe runs about nine
// seconds before there is a session. It correctly sent no credential, the
// server correctly answered 401, and the probe correctly failed closed — and
// then CACHED that, so logging in never re-ran it. Every download afterwards
// went down the browser path.
//
// 404 means "this binary has no such route" and is a fact about the BINARY,
// which is why caching it is right. 401 means "you are not signed in", a fact
// about the SESSION, which changes. The two must not be conflated.
test('a 401 is not cached, because it is a fact about the session not the binary', async () => {
  resetDesktopProbe();
  __setAccessTokenForTests(null);

  let calls = 0;
  const impl = (async () => {
    calls++;
    // Unauthenticated first, exactly as the real server answers.
    if (calls === 1) return jsonResponse({ error: 'unauthorized' }, 401);
    return jsonResponse({ desktop_downloads: true, download_dir: '/d' });
  }) as unknown as typeof fetch;

  const before = await probeDesktop(impl);
  assert.equal(before.desktopDownloads, false, 'an unauthenticated probe must fail closed');

  // Now a session exists, as after login.
  __setAccessTokenForTests(TOKEN);
  const after = await probeDesktop(impl);

  assert.equal(calls, 2, 'the probe did not re-run after the 401; it cached a session failure');
  assert.equal(
    after.desktopDownloads,
    true,
    'the desktop path stayed disabled after login because the pre-auth 401 was cached',
  );
});

// A 404 still caches: that one really is a property of the binary.
test('a 404 is cached, because the binary cannot grow the route while running', async () => {
  resetDesktopProbe();
  __setAccessTokenForTests(TOKEN);

  let calls = 0;
  const impl = (async () => {
    calls++;
    return jsonResponse({}, 404);
  }) as unknown as typeof fetch;

  await probeDesktop(impl);
  await probeDesktop(impl);
  await probeDesktop(impl);

  assert.equal(calls, 1, 'the browser build re-probed; a 404 is stable and should be cached');
});
