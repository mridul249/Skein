import assert from 'node:assert/strict';
import test from 'node:test';

import { probeDesktop, resetDesktopProbe } from '../src/lib/desktop';
import { formatEta, isTerminal } from '../src/lib/desktop-downloads';

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

test('the desktop build is detected from the capability route', async () => {
  resetDesktopProbe();
  const caps = await probeDesktop(
    async () => jsonResponse({ desktop_downloads: true, download_dir: '/home/x/Downloads' }),
  );
  assert.equal(caps.desktopDownloads, true);
  assert.equal(caps.downloadDir, '/home/x/Downloads');
});

// THE PROBE FAILS CLOSED. Every one of these means "browser", which means the
// existing a.click() path, unchanged.
test('anything other than an explicit true means browser', async () => {
  const cases: { name: string; respond: () => Promise<Response> }[] = [
    { name: '404 (the browser build)', respond: async () => jsonResponse({}, 404) },
    { name: '500', respond: async () => jsonResponse({ desktop_downloads: true }, 500) },
    { name: 'missing field', respond: async () => jsonResponse({ download_dir: '/x' }) },
    { name: 'truthy but not boolean', respond: async () => jsonResponse({ desktop_downloads: 'yes' }) },
    { name: 'explicit false', respond: async () => jsonResponse({ desktop_downloads: false }) },
    { name: 'null body', respond: async () => jsonResponse(null) },
    { name: 'not JSON', respond: async () => new Response('<html>', { status: 200 }) },
    {
      name: 'network failure',
      respond: async () => {
        throw new Error('connection refused');
      },
    },
  ];

  for (const tc of cases) {
    resetDesktopProbe();
    const caps = await probeDesktop(tc.respond as unknown as typeof fetch);
    assert.equal(
      caps.desktopDownloads,
      false,
      `${tc.name} enabled the desktop path; the probe must fail closed`,
    );
  }
});

test('a hanging probe times out and falls back to browser', async () => {
  resetDesktopProbe();
  const hang = ((_url: string, init?: RequestInit) =>
    new Promise<Response>((_resolve, reject) => {
      init?.signal?.addEventListener('abort', () => reject(new Error('aborted')));
    })) as unknown as typeof fetch;

  const caps = await probeDesktop(hang, 20);
  assert.equal(caps.desktopDownloads, false, 'a hanging probe must not block on the desktop path');
});

test('the answer is cached, because a binary cannot change while running', async () => {
  resetDesktopProbe();
  let calls = 0;
  const counting = (async () => {
    calls++;
    return jsonResponse({ desktop_downloads: true, download_dir: '/d' });
  }) as unknown as typeof fetch;

  await probeDesktop(counting);
  await probeDesktop(counting);
  await probeDesktop(counting);
  assert.equal(calls, 1, 'the probe re-ran; it should be cached');
});

// -1 MEANS UNKNOWN AND RENDERS BLANK. "0s" would read as "finished".
test('an unknown ETA renders blank, not 0s', () => {
  assert.equal(formatEta(-1), '', 'an unknown ETA must render blank');
  assert.equal(formatEta(0), '1s', 'a real zero rounds up rather than reading as done');
  assert.equal(formatEta(45), '45s');
  assert.equal(formatEta(90), '1m 30s');
  assert.equal(formatEta(3700), '1h 1m');
});

test('terminal jobs are the dismissible ones', () => {
  const base = {
    id: 'dl-1', file_id: 'f', name: 'x', path: '/x', done: 0, total: 10,
    bytes_per_sec: 0, eta_seconds: -1,
  };
  assert.equal(isTerminal({ ...base, state: 'running' }), false);
  assert.equal(isTerminal({ ...base, state: 'complete' }), true);
  assert.equal(isTerminal({ ...base, state: 'failed' }), true);
  assert.equal(isTerminal({ ...base, state: 'cancelled' }), true);
});
