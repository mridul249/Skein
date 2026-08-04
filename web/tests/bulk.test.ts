import assert from 'node:assert/strict';
import test from 'node:test';

import {
  failureReasons,
  runBulkDownload,
  summarise,
  type BulkOutcome,
} from '../src/lib/bulk';

function outcome(succeeded: number, failures: { id: string; error: string }[]): BulkOutcome {
  const results = [
    ...Array.from({ length: succeeded }, (_, i) => ({ file_id: `ok${i}`, ok: true })),
    ...failures.map((f) => ({ file_id: f.id, ok: false, code: 'failed', error: f.error })),
  ];
  return {
    results,
    succeeded,
    failed: failures.length,
    failedIds: failures.map((f) => f.id),
  };
}

// PARTIAL FAILURE IS NOT A SUCCESS TOAST AND NOT AN ERROR TOAST.
test('a partial failure names both counts', () => {
  const msg = summarise(outcome(7, [
    { id: 'a', error: 'A drive is rate limiting.' },
    { id: 'b', error: 'A drive is rate limiting.' },
    { id: 'c', error: 'That file no longer exists.' },
  ]));

  assert.ok(msg.includes('7 of 10'), `message must say how many of how many: ${msg}`);
  assert.ok(msg.includes('3 failed'), `message must name the failure count: ${msg}`);
});

test('a clean run reads as a plain success', () => {
  assert.equal(summarise(outcome(3, [])), 'Deleted 3 files.');
  assert.equal(summarise(outcome(1, [])), 'Deleted 1 file.');
});

test('a total failure does not claim any successes', () => {
  const msg = summarise(outcome(0, [{ id: 'a', error: 'nope' }]));
  assert.ok(msg.includes('Could not delete 1 file'), msg);
  assert.ok(!msg.includes('Deleted'), `a total failure must not claim successes: ${msg}`);
});

// Failures are grouped so ten rate-limited files are one line, not ten.
test('failure reasons are grouped and ordered by frequency', () => {
  const reasons = failureReasons(outcome(1, [
    { id: 'a', error: 'A drive is rate limiting.' },
    { id: 'b', error: 'That file no longer exists.' },
    { id: 'c', error: 'A drive is rate limiting.' },
    { id: 'd', error: 'A drive is rate limiting.' },
  ]));

  assert.equal(reasons.length, 2);
  assert.deepEqual(reasons[0], { reason: 'A drive is rate limiting.', count: 3 });
  assert.deepEqual(reasons[1], { reason: 'That file no longer exists.', count: 1 });
});

test('failed ids are exposed so they can stay selected for a retry', () => {
  const o = outcome(2, [{ id: 'x', error: 'boom' }, { id: 'y', error: 'boom' }]);
  assert.deepEqual(o.failedIds, ['x', 'y']);
});

// Sequential, staggered, and one failure must not stop the rest.
test('bulk download starts every file in order', async () => {
  const seen: string[] = [];
  const files = [
    { id: '1', name: 'a.bin' },
    { id: '2', name: 'b.bin' },
    { id: '3', name: 'c.bin' },
  ];

  const out = await runBulkDownload(
    files,
    async (f) => {
      seen.push(f.id);
    },
    { staggerMs: 0 },
  );

  assert.deepEqual(seen, ['1', '2', '3'], 'downloads must run in order');
  assert.equal(out.started, 3);
  assert.equal(out.failed.length, 0);
});

test('one failed download does not abandon the rest', async () => {
  const files = [
    { id: '1', name: 'a.bin' },
    { id: '2', name: 'b.bin' },
    { id: '3', name: 'c.bin' },
  ];

  const out = await runBulkDownload(
    files,
    async (f) => {
      if (f.id === '2') throw new Error('capability mint failed');
    },
    { staggerMs: 0 },
  );

  assert.equal(out.started, 2, 'the files after the failure must still start');
  assert.equal(out.failed.length, 1);
  assert.equal(out.failed[0]?.id, '2');
  assert.equal(out.failed[0]?.error, 'capability mint failed');
});

test('bulk download reports progress per file', async () => {
  const seen: number[] = [];
  await runBulkDownload(
    [{ id: '1', name: 'a' }, { id: '2', name: 'b' }],
    async () => undefined,
    { staggerMs: 0, onProgress: (done) => seen.push(done) },
  );
  assert.deepEqual(seen, [1, 2]);
});
