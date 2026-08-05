import type { FileItem } from '../lib/api';
import { evidenceAge, fileHealth } from '../lib/health';

/**
 * The damaged badge.
 *
 * Marks a file whose shards a COMPLETE reconcile run confirmed missing. Health
 * is read from the persisted status, so the badge survives a page reload —
 * before the schema bundle it lived only in one reconcile response and vanished
 * on the next render.
 *
 * INTEGRITY IS NEVER CARRIED BY COLOUR ALONE (Design.md, and known issue #30:
 * the account ramp collides with the semantic colours under every dichromacy
 * tested). The glyph and the word "Damaged" do the work; the colour is
 * reinforcement.
 */
export function DamagedBadge({ file }: { file: FileItem }) {
  const health = fileHealth(file);
  if (health !== 'partially_missing' && health !== 'corrupted') return null;

  // Two states, two truths. "Partially missing" still has surviving shards;
  // "corrupted" has none, and telling a user their file is partly there when
  // nothing is would be worse than saying nothing.
  const label = health === 'corrupted' ? 'All shards missing' : 'Shards missing';
  const age = evidenceAge(file);

  return (
    <span
      data-damaged={health}
      className="ml-2 inline-flex shrink-0 items-center gap-1 rounded border border-danger/40 bg-danger/10 px-1.5 py-0.5 align-middle text-caption text-danger"
      title={
        age
          ? `${label}. Last checked ${age}.`
          : `${label}. This file has not been checked since it was uploaded.`
      }
    >
      <span aria-hidden="true">✕</span>
      Damaged
      <span className="sr-only">
        {age ? `: ${label}, last checked ${age}` : `: ${label}, never checked`}
      </span>
    </span>
  );
}
