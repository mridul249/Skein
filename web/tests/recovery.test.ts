import assert from 'node:assert/strict';
import test from 'node:test';

import type { BackfillReport, ReconstructReport } from '../src/lib/api';
import {
  coverageSummary,
  coverageVerdict,
  restoreSummary,
  unscannedAccounts,
} from '../src/lib/recovery';

function report(over: Partial<BackfillReport> = {}): BackfillReport {
  return {
    dry_run: true,
    complete: true,
    accounts: [{ account_id: 'a1', scanned: true, manifests_found: 0 }],
    coverage: {
      files: 10,
      covered: 10,
      partially_covered: 0,
      uncovered: 0,
      damaged: 0,
      indeterminate: 0,
    },
    results: [],
    ...over,
  };
}

function restore(over: Partial<ReconstructReport> = {}): ReconstructReport {
  return {
    dry_run: false,
    complete: true,
    accounts: [],
    manifests_found: 0,
    manifests_unreadable: 0,
    files_recovered: 0,
    shards_recovered: 0,
    folders_recovered: 0,
    files_already_present: 0,
    ...over,
  };
}

test('full coverage reads as full', () => {
  assert.equal(coverageVerdict(report()), 'full');
});

test('no coverage reads as none', () => {
  const r = report({ coverage: { ...report().coverage, covered: 0, uncovered: 10 } });
  assert.equal(coverageVerdict(r), 'none');
});

test('partial coverage reads as partial', () => {
  const r = report({ coverage: { ...report().coverage, covered: 4, uncovered: 6 } });
  assert.equal(coverageVerdict(r), 'partial');
});

// THE ONE THAT MATTERS. An incomplete scan must never read as a coverage
// figure, in either direction: a user told "4 of 10 covered" acts on a number
// that was never established.
test('an incomplete scan is unknown, not partial', () => {
  const r = report({
    complete: false,
    coverage: { ...report().coverage, covered: 4, indeterminate: 6 },
  });
  assert.equal(coverageVerdict(r), 'unknown');
  assert.match(coverageSummary(r), /could not be checked/);
});

test('an unreachable drive makes coverage unknown even when the run claims complete', () => {
  const r = report({ coverage: { ...report().coverage, covered: 4, indeterminate: 6 } });
  assert.equal(coverageVerdict(r), 'unknown');
});

// Damaged files are excluded from backfill by design, so they must not drag
// coverage below full — otherwise a library with one dead file can never read
// as protected however many manifests exist.
test('damaged files do not count against coverage', () => {
  const r = report({
    coverage: { ...report().coverage, files: 10, covered: 9, damaged: 1 },
  });
  assert.equal(coverageVerdict(r), 'full');
  assert.match(coverageSummary(r), /All 9 files/);
});

test('an empty library says so rather than claiming protection', () => {
  const r = report({
    coverage: { files: 0, covered: 0, partially_covered: 0, uncovered: 0, damaged: 0, indeterminate: 0 },
  });
  assert.match(coverageSummary(r), /no files to protect/);
});

test('a null report reads as checking, never as covered', () => {
  assert.equal(coverageVerdict(null), 'unknown');
  assert.match(coverageSummary(null), /Checking/);
});

test('unscanned drives are named, not swallowed', () => {
  const r = report({
    complete: false,
    accounts: [
      { account_id: 'a1', scanned: true, manifests_found: 3 },
      { account_id: 'a2', scanned: false, manifests_found: 0, reason: 'the drive was rate limiting' },
    ],
  });
  const reasons = unscannedAccounts(r);
  assert.equal(reasons.length, 1);
  assert.match(reasons[0]!, /rate limiting/);
});

test('a restore that recovered nothing says so plainly', () => {
  const lines = restoreSummary(restore());
  assert.match(lines.join(' '), /Nothing was recovered/);
});

test('a restore reports what it recovered', () => {
  const lines = restoreSummary(
    restore({ files_recovered: 3, shards_recovered: 13, folders_recovered: 1 }),
  );
  assert.match(lines.join(' '), /Recovered 3 files, 13 shards and 1 folder/);
});

// INCOMPLETENESS GETS ITS OWN SENTENCE. Folding it into a success line is how
// a user stops looking for the rest of their library.
test('an incomplete restore states incompleteness separately from the count', () => {
  const lines = restoreSummary(restore({ files_recovered: 12, complete: false }));
  assert.match(lines[0]!, /Recovered 12 files/);
  assert.ok(
    lines.some((l) => /incomplete/.test(l)),
    'an incomplete run must say so in its own line, not hidden inside the success sentence',
  );
});

test('unreadable manifests are reported as files that were NOT recovered', () => {
  const lines = restoreSummary(restore({ files_recovered: 2, manifests_unreadable: 3 }));
  assert.match(lines.join(' '), /3 manifests could not be read/);
  assert.match(lines.join(' '), /exist but were not recovered/);
});

test('already-present files are distinguished from newly recovered ones', () => {
  const lines = restoreSummary(restore({ files_recovered: 0, files_already_present: 7 }));
  assert.match(lines.join(' '), /already in the database and left untouched/);
});
