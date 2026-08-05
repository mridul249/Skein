import assert from 'node:assert/strict';
import test from 'node:test';

import type { FileItem } from '../src/lib/api';
import {
  damagedFiles,
  evidenceAge,
  fileHealth,
  isDamaged,
  splitDownloadable,
} from '../src/lib/health';

function file(over: Partial<FileItem> = {}): FileItem {
  return {
    id: 'f1',
    name: 'a.bin',
    folder_id: null,
    size_bytes: 10,
    is_striped: false,
    is_encrypted: false,
    status: 'ready',
    created_at: '2026-08-01T00:00:00Z',
    updated_at: '2026-08-01T00:00:00Z',
    reconciled_at: null,
    shards: [],
    ...over,
  };
}

test('a ready file is healthy', () => {
  assert.equal(fileHealth(file()), 'ok');
  assert.equal(isDamaged(file()), false);
});

test('the two reconciled damage states are damaged', () => {
  for (const status of ['partially_missing', 'corrupted']) {
    const f = file({ status });
    assert.equal(isDamaged(f), true, `${status} should be damaged`);
    assert.equal(fileHealth(f), status);
  }
});

// An unknown status must not read as healthy. A future status this build has
// never heard of is exactly when a UI should stay quiet rather than assert
// that everything is fine.
test('an unrecognised status is not reported as healthy', () => {
  assert.notEqual(fileHealth(file({ status: 'something_new' })), 'ok');
});

// THE ONE THAT PROTECTS DATA. A damaged file must never be handed to a bulk
// download: its shards are gone, the server refuses it with a 409, and
// including it means a "download 20 files" action reports failures the user
// cannot act on.
test('bulk download excludes damaged files and says which it skipped', () => {
  const files = [
    file({ id: 'ok1' }),
    file({ id: 'bad1', status: 'partially_missing' }),
    file({ id: 'ok2' }),
    file({ id: 'bad2', status: 'corrupted' }),
  ];

  const { downloadable, skipped } = splitDownloadable(files);

  assert.deepEqual(
    downloadable.map((f) => f.id),
    ['ok1', 'ok2'],
  );
  assert.deepEqual(
    skipped.map((f) => f.id),
    ['bad1', 'bad2'],
  );
});

test('splitDownloadable leaves a healthy selection untouched', () => {
  const files = [file({ id: 'a' }), file({ id: 'b' })];
  const { downloadable, skipped } = splitDownloadable(files);
  assert.equal(downloadable.length, 2);
  assert.equal(skipped.length, 0);
});

test('damagedFiles picks out exactly the damaged rows', () => {
  const files = [file({ id: 'a' }), file({ id: 'b', status: 'corrupted' })];
  assert.deepEqual(
    damagedFiles(files).map((f) => f.id),
    ['b'],
  );
});

// reconciled_at exists so a badge can say how old its evidence is. Null means
// never checked, which must NOT render as "checked just now".
test('evidence age distinguishes never-checked from checked', () => {
  assert.equal(evidenceAge(file({ reconciled_at: null })), null);

  const now = new Date('2026-08-06T12:00:00Z');
  const checked = file({ reconciled_at: '2026-08-06T11:00:00Z' });
  const age = evidenceAge(checked, now);
  assert.equal(age, '1 hour ago');
});

test('evidence age reads as recent for a check moments ago', () => {
  const now = new Date('2026-08-06T12:00:00Z');
  const age = evidenceAge(file({ reconciled_at: '2026-08-06T11:59:30Z' }), now);
  assert.equal(age, 'just now');
});
