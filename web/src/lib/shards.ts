import { shardColor } from './format';

/**
 * Integrity state of one shard. Design.md §5.
 *
 * **Only two of these three are reachable today, and that is deliberate.**
 *
 * The API reports `index`, `account_id`, `size_bytes`, `plain_size_bytes` and
 * `plain_offset` per shard — and nothing about integrity. Nothing in the
 * system has checked that a shard is still present and intact at the provider,
 * so nothing may render as `verified`.
 *
 * That is not an oversight to paper over in the UI. The server *cannot* verify
 * a shard cheaply yet: `file_shards.sha256` is over the **plaintext**, so
 * confirming a provider object matches would mean downloading and decrypting
 * it. Known issue #9 records the fix — a per-shard **ciphertext** digest,
 * captured at upload — and Phase 7 Task 6's `doctor` is what would then set
 * this state. When that lands, this function grows a real third branch and
 * every call site already handles it.
 *
 * Until then a shard is `unverified`, which is exactly true: it is recorded,
 * and no one has looked. Painting a green tick on an unchecked shard is the
 * same class of small lie as a progress bar sitting at 100% while the server
 * is still writing.
 */
export type ShardState = 'verified' | 'unverified' | 'missing';

interface Rampable {
  id: string;
  ordinal: number;
}

export function shardState(accountId: string | null, drives: Rampable[]): ShardState {
  // Unresolvable account: orphaned by a disconnect (#19), or on a drive this
  // caller was not given. Either way the shard cannot be read.
  if (shardColor(accountId, drives) === null) return 'missing';
  return 'unverified';
}

/** How each state reads. Never colour alone — the glyph carries it too. */
export const SHARD_STATE: Record<
  ShardState,
  { glyph: string; word: string; tone: string; hint: string }
> = {
  verified: {
    glyph: '✓',
    word: 'Verified',
    tone: 'text-success',
    hint: 'Checked against the copy on the drive.',
  },
  unverified: {
    glyph: '·',
    word: 'Unverified',
    tone: 'text-muted',
    hint: 'Recorded here, not yet checked against the drive.',
  },
  missing: {
    glyph: '✕',
    word: 'Missing',
    tone: 'text-danger',
    hint: 'The drive holding this shard is not connected.',
  },
};

/** A one-line summary of a file's shards, for the detail header. */
export function integritySummary(states: ShardState[]): {
  word: string;
  tone: string;
} {
  const missing = states.filter((s) => s === 'missing').length;
  if (missing > 0) {
    return {
      // Plural agrees with the total, not the missing count: "1 of 3
      // shard unreachable" is what the other way round produces.
      word: `${missing} of ${states.length} ${states.length === 1 ? 'shard' : 'shards'} unreachable`,
      tone: 'text-danger',
    };
  }
  const verified = states.filter((s) => s === 'verified').length;
  if (verified === states.length) {
    return { word: `All ${states.length} shards verified`, tone: 'text-success' };
  }
  return {
    word: `${states.length} ${states.length === 1 ? 'shard' : 'shards'} recorded, none verified yet`,
    tone: 'text-muted',
  };
}
