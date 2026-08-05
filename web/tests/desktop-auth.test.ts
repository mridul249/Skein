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
