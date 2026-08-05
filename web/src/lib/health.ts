/**
 * File health, derived from the persisted status.
 *
 * Reconcile used to return damage only in its own response body, so a badge
 * was correct for one render and gone on the next page load. The schema bundle
 * widened `files.status` to carry `partially_missing` and `corrupted` and added
 * `reconciled_at`, so health is now a property of the row and this module is
 * the single place the frontend interprets it.
 *
 * Pure functions over plain data, deliberately: this is the logic tier, so it
 * is covered by `node --test` rather than needing a browser.
 */
import type { FileItem } from './api';

/** The health states a listing row can be in. */
export type Health = 'ok' | 'partially_missing' | 'corrupted' | 'unknown';

/**
 * fileHealth maps a persisted status onto a health state.
 *
 * An UNRECOGNISED status returns 'unknown', never 'ok'. A build that has not
 * heard of a future status must stay quiet rather than assert the file is
 * fine — claiming health you cannot vouch for is the failure this whole
 * feature exists to avoid.
 */
export function fileHealth(file: FileItem): Health {
  switch (file.status) {
    case 'ready':
      return 'ok';
    case 'partially_missing':
      return 'partially_missing';
    case 'corrupted':
      return 'corrupted';
    default:
      return 'unknown';
  }
}

/** True when shards are confirmed missing, so the file cannot be read whole. */
export function isDamaged(file: FileItem): boolean {
  const h = fileHealth(file);
  return h === 'partially_missing' || h === 'corrupted';
}

/** The damaged rows of a listing, in order. */
export function damagedFiles(files: FileItem[]): FileItem[] {
  return files.filter(isDamaged);
}

/**
 * splitDownloadable separates what can be fetched from what cannot.
 *
 * A damaged file must never reach a bulk download. Its shards are gone, the
 * server refuses to sign a capability URL for it (409), and including it turns
 * "download 20 files" into a pile of failures the user can do nothing about.
 * Skipping it up front, and SAYING so, is the honest behaviour — silently
 * dropping it would be the other way to get this wrong.
 */
export function splitDownloadable(files: FileItem[]): {
  downloadable: FileItem[];
  skipped: FileItem[];
} {
  const downloadable: FileItem[] = [];
  const skipped: FileItem[] = [];
  for (const f of files) {
    (isDamaged(f) ? skipped : downloadable).push(f);
  }
  return { downloadable, skipped };
}

/**
 * evidenceAge renders how long ago this file's health was established, or null
 * if it never has been.
 *
 * The null case is the point. "Never checked" and "checked and healthy" are
 * different facts, and a badge that renders them the same invites trusting
 * evidence that was never gathered.
 */
export function evidenceAge(file: FileItem, now: Date = new Date()): string | null {
  if (!file.reconciled_at) return null;

  const then = new Date(file.reconciled_at).getTime();
  if (Number.isNaN(then)) return null;

  const seconds = Math.max(0, Math.round((now.getTime() - then) / 1000));
  if (seconds < 60) return 'just now';

  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes} minute${minutes === 1 ? '' : 's'} ago`;

  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours} hour${hours === 1 ? '' : 's'} ago`;

  const days = Math.floor(hours / 24);
  return `${days} day${days === 1 ? '' : 's'} ago`;
}
