import clsx from 'clsx';

import type { Drive, FileItem } from '../lib/api';
import { shardOrdinal } from '../lib/format';
import { AccountChip } from './AccountChip';

/**
 * The compact shard indicator in a file row: one numbered chip per shard, in
 * index order.
 *
 * **This used to be a hover popover and no longer is.** The expanded map moved
 * into the detail drawer, which removes a whole class of defect rather than
 * working around it (known issue #27). A panel anchored inside the file table
 * was clipped by the card's `overflow-x: auto`; opening it grew the card's
 * scroll height, raised a scrollbar, narrowed the content box and shifted this
 * very trigger 10px out from under the pointer that had just opened it. A
 * drawer lives outside the table entirely, so none of that geometry exists to
 * get wrong.
 *
 * What stays is the glance: how many shards, on which accounts, in what order.
 * Design.md §5 — addresses, not fragments.
 */
export function ShardMap({
  file,
  drives,
  activeShard,
}: {
  file: FileItem;
  drives: Drive[];
  /** The shard currently being written, if this file is uploading. */
  activeShard?: number;
}) {
  if (file.shards.length === 0) {
    return <span className="text-data-sm text-faint">—</span>;
  }

  const across = new Set(
    file.shards.map((s) => shardOrdinal(s.account_id, drives)).filter((n) => n !== null),
  ).size;

  return (
    <span
      className="inline-flex items-center gap-1"
      role="img"
      aria-label={`${file.shards.length} ${
        file.shards.length === 1 ? 'shard' : 'shards'
      } across ${across} ${across === 1 ? 'drive' : 'drives'}`}
    >
      {file.shards.map((shard) => (
        <AccountChip
          key={shard.index}
          ordinal={shardOrdinal(shard.account_id, drives)}
          size="sm"
          className={clsx(activeShard === shard.index && 'shard-active')}
        />
      ))}
    </span>
  );
}
