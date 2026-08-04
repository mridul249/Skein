/**
 * File selection, kept out of React so it can be tested without a DOM.
 *
 * The rules the UI depends on:
 *
 *   - Selection survives sorting and filtering. It is a set of ids, never a
 *     set of row indices, so reordering the list cannot silently reassign what
 *     is selected to different files.
 *   - Selection clears on navigation. Carrying a selection from Files into
 *     Trash and then pressing Delete would act on rows the user can no longer
 *     see.
 *   - Ids that leave the list are dropped, so a file deleted in another tab
 *     cannot linger in the selection and reappear in the next bulk request.
 */

/** The server's per-request cap, mirrored so the client can chunk. */
export const BULK_LIMIT = 200;

export interface SelectionState {
  ids: ReadonlySet<string>;
  /** The row a shift-click ranges from. */
  anchor: string | null;
}

export const EMPTY: SelectionState = { ids: new Set(), anchor: null };

export function isSelected(state: SelectionState, id: string): boolean {
  return state.ids.has(id);
}

export function count(state: SelectionState): number {
  return state.ids.size;
}

/** toggle flips one row and moves the shift-range anchor to it. */
export function toggle(state: SelectionState, id: string): SelectionState {
  const ids = new Set(state.ids);
  if (ids.has(id)) ids.delete(id);
  else ids.add(id);
  return { ids, anchor: id };
}

/**
 * selectRange adds every row between the anchor and id inclusive.
 *
 * Shift-click ADDS rather than replaces: a user building a selection in two
 * runs would otherwise lose the first one. Falls back to a plain toggle when
 * there is no anchor or either end has left the list.
 */
export function selectRange(
  state: SelectionState,
  id: string,
  visible: readonly string[],
): SelectionState {
  if (!state.anchor) return toggle(state, id);

  const from = visible.indexOf(state.anchor);
  const to = visible.indexOf(id);
  if (from < 0 || to < 0) return toggle(state, id);

  const [lo, hi] = from <= to ? [from, to] : [to, from];
  const ids = new Set(state.ids);
  for (let i = lo; i <= hi; i++) {
    const rowId = visible[i];
    if (rowId !== undefined) ids.add(rowId);
  }
  // The anchor stays put, so a second shift-click re-ranges from the same
  // origin rather than from wherever the last one ended.
  return { ids, anchor: state.anchor };
}

/** selectAll selects every visible row, replacing whatever was selected. */
export function selectAll(visible: readonly string[]): SelectionState {
  return { ids: new Set(visible), anchor: null };
}

/** clear empties the selection. */
export function clear(): SelectionState {
  return EMPTY;
}

/** keepOnly drops ids that are no longer in the list. */
export function keepOnly(
  state: SelectionState,
  visible: readonly string[],
): SelectionState {
  const live = new Set(visible);
  let changed = false;
  const ids = new Set<string>();
  for (const id of state.ids) {
    if (live.has(id)) ids.add(id);
    else changed = true;
  }
  if (!changed) return state;
  return {
    ids,
    anchor: state.anchor && live.has(state.anchor) ? state.anchor : null,
  };
}

/** replace sets the selection to exactly these ids. Used to keep failures selected. */
export function replace(ids: readonly string[]): SelectionState {
  return { ids: new Set(ids), anchor: null };
}

/** The header checkbox has three states, not two. */
export type HeaderState = 'none' | 'some' | 'all';

export function headerState(
  state: SelectionState,
  visible: readonly string[],
): HeaderState {
  if (visible.length === 0 || state.ids.size === 0) return 'none';
  let selected = 0;
  for (const id of visible) if (state.ids.has(id)) selected++;
  if (selected === 0) return 'none';
  return selected === visible.length ? 'all' : 'some';
}

/**
 * chunk splits ids into request-sized batches.
 *
 * The server refuses more than BULK_LIMIT per call. Chunking here rather than
 * disabling select-all past the cap means a 201-file selection works instead
 * of failing opaquely — which is the behaviour the cap would otherwise produce
 * for a user who has no idea the limit exists.
 */
export function chunk(ids: readonly string[], size = BULK_LIMIT): string[][] {
  if (ids.length === 0) return [];
  const out: string[][] = [];
  for (let i = 0; i < ids.length; i += size) {
    out.push(ids.slice(i, i + size));
  }
  return out;
}
