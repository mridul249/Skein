import assert from 'node:assert/strict';
import test from 'node:test';

import {
  BULK_LIMIT,
  EMPTY,
  chunk,
  count,
  headerState,
  isSelected,
  keepOnly,
  replace,
  selectAll,
  selectRange,
  toggle,
} from '../src/lib/selection';

const rows = ['a', 'b', 'c', 'd', 'e'];

test('toggle selects and deselects one row', () => {
  let s = toggle(EMPTY, 'b');
  assert.ok(isSelected(s, 'b'));
  assert.equal(count(s), 1);

  s = toggle(s, 'b');
  assert.ok(!isSelected(s, 'b'));
  assert.equal(count(s), 0);
});

// SELECTION IS BY ID, NOT BY INDEX. Sorting the list must not silently
// reassign the selection to different files.
test('selection survives reordering', () => {
  const s = toggle(toggle(EMPTY, 'a'), 'd');

  const resorted = ['e', 'd', 'c', 'b', 'a'];
  const after = keepOnly(s, resorted);

  assert.ok(isSelected(after, 'a'));
  assert.ok(isSelected(after, 'd'));
  assert.equal(count(after), 2);
});

// Filtering hides rows; the ones that vanish must not stay selected, or the
// next bulk request acts on files the user cannot see.
test('ids that leave the list are dropped', () => {
  const s = toggle(toggle(toggle(EMPTY, 'a'), 'b'), 'c');

  const filtered = ['a', 'c'];
  const after = keepOnly(s, filtered);

  assert.equal(count(after), 2);
  assert.ok(isSelected(after, 'a'));
  assert.ok(isSelected(after, 'c'));
  assert.ok(!isSelected(after, 'b'), 'a filtered-out row stayed selected');
});

test('keepOnly returns the same object when nothing changed', () => {
  const s = toggle(EMPTY, 'a');
  assert.equal(keepOnly(s, rows), s, 'a no-op must not produce a new identity');
});

test('shift-range adds every row between the anchor and the target', () => {
  const s = toggle(EMPTY, 'b'); // anchor at b
  const ranged = selectRange(s, 'd', rows);

  assert.deepEqual([...ranged.ids].sort(), ['b', 'c', 'd']);
});

test('shift-range works backwards', () => {
  const s = toggle(EMPTY, 'd');
  const ranged = selectRange(s, 'b', rows);
  assert.deepEqual([...ranged.ids].sort(), ['b', 'c', 'd']);
});

// A user building a selection in two runs must not lose the first.
test('shift-range adds to an existing selection rather than replacing it', () => {
  let s = toggle(EMPTY, 'a');
  s = toggle(s, 'c'); // anchor moves to c
  const ranged = selectRange(s, 'e', rows);

  assert.ok(isSelected(ranged, 'a'), 'the earlier selection was discarded');
  assert.deepEqual([...ranged.ids].sort(), ['a', 'c', 'd', 'e']);
});

test('shift-range with no anchor behaves as a plain toggle', () => {
  const ranged = selectRange(EMPTY, 'c', rows);
  assert.deepEqual([...ranged.ids], ['c']);
});

test('shift-range falls back to a toggle when the anchor has left the list', () => {
  const s = toggle(EMPTY, 'z'); // anchor on a row not in `rows`
  const ranged = selectRange(s, 'c', rows);
  assert.ok(isSelected(ranged, 'c'));
});

// The header checkbox is tri-state. "some" is what drives `indeterminate`.
test('header state reflects none, some and all', () => {
  assert.equal(headerState(EMPTY, rows), 'none');
  assert.equal(headerState(toggle(EMPTY, 'a'), rows), 'some');
  assert.equal(headerState(selectAll(rows), rows), 'all');
});

test('header state is none when the list is empty', () => {
  assert.equal(headerState(selectAll(rows), []), 'none');
});

// Selecting all of a FILTERED list, then widening the filter, must read as
// "some" rather than claiming everything is selected.
test('header state is some when the visible list grows past the selection', () => {
  const s = selectAll(['a', 'b']);
  assert.equal(headerState(s, rows), 'some');
});

test('replace keeps exactly the given ids, for retrying failures', () => {
  const s = replace(['b', 'd']);
  assert.deepEqual([...s.ids].sort(), ['b', 'd']);
  assert.equal(s.anchor, null);
});

// THE 200-CAP. A 201-file selection must not fail opaquely.
test('chunk splits a selection into request-sized batches', () => {
  const many = Array.from({ length: 201 }, (_, i) => `f${i}`);
  const batches = chunk(many);

  assert.equal(batches.length, 2);
  assert.equal(batches[0]?.length, BULK_LIMIT);
  assert.equal(batches[1]?.length, 1);

  const flat = batches.flat();
  assert.equal(flat.length, 201, 'chunking lost or duplicated ids');
  assert.equal(new Set(flat).size, 201);
});

test('chunk returns one batch below the cap and none when empty', () => {
  assert.equal(chunk(['a', 'b']).length, 1);
  assert.deepEqual(chunk([]), []);
});
