import { useState } from 'react';
import { Lock } from 'lucide-react';
import clsx from 'clsx';

import type { Drive, FileItem } from '../lib/api';
import {
  DRIVE_BG,
  DRIVE_TEXT,
  UNKNOWN_DRIVE_BG,
  UNKNOWN_DRIVE_TEXT,
  bytes,
  shardColor,
} from '../lib/format';

/**
 * The shard map. Design.md §5.
 *
 * This is the one element the product is remembered by, and the only place to
 * spend visual boldness. Every competing tool hides the underlying accounts
 * behind a unified list; Skein's entire reason to exist is that a file is
 * physically distributed, so the interface shows that rather than abstracting
 * it away.
 *
 * Compact form is one dot per shard in its drive's colour. Hover expands it
 * into the full map: a bar proportional to real shard sizes, segmented in
 * drive colours, plus the per-shard breakdown.
 */

interface Props {
  file: FileItem;
  drives: Drive[];
  /** The shard currently being written, if this file is uploading. */
  activeShard?: number;
}

export function ShardMap({ file, drives, activeShard }: Props) {
  const [open, setOpen] = useState(false);

  // Returns null for a shard whose drive cannot be identified. This used to
  // be `?? 1`, which painted an orphaned shard in the first drive's colour —
  // the map's whole job is saying which drive holds what, so a confident wrong
  // answer was worse than none.
  const bgFor = (accountId: string | null): string => {
    const color = shardColor(accountId, drives);
    return color ? DRIVE_BG[color] : UNKNOWN_DRIVE_BG;
  };

  const textFor = (accountId: string | null): string => {
    const color = shardColor(accountId, drives);
    return color ? DRIVE_TEXT[color] : UNKNOWN_DRIVE_TEXT;
  };

  const driveLabel = (accountId: string | null): string => {
    const drive = drives.find((d) => d.id === accountId);
    if (!drive) return 'unknown drive';
    return drive.email || drive.display_name || 'drive';
  };

  if (file.shards.length === 0) {
    return <span className="text-data-sm text-subtext0">—</span>;
  }

  return (
    <div
      className="relative inline-flex"
      onMouseEnter={() => setOpen(true)}
      onMouseLeave={() => setOpen(false)}
    >
      <button
        type="button"
        className="flex items-center gap-1 px-1 py-1"
        aria-label={`${file.shards.length} ${
          file.shards.length === 1 ? 'shard' : 'shards'
        } across ${new Set(file.shards.map((s) => s.account_id)).size} drives`}
        aria-expanded={open}
        onClick={() => setOpen((v) => !v)}
        onFocus={() => setOpen(true)}
        onBlur={() => setOpen(false)}
      >
        {file.shards.map((shard) => (
          <span
            key={shard.index}
            className={clsx(
              'h-2 w-2',
              bgFor(shard.account_id),
              activeShard === shard.index && 'shard-active',
            )}
          />
        ))}
      </button>

      {open && (
        <div
          role="tooltip"
          className="absolute right-0 top-full z-20 mt-2 w-[30rem] max-w-[90vw]
                     border border-surface0 bg-base p-2ch shadow-none"
        >
          <div className="mb-3 flex items-baseline justify-between gap-3">
            <span className="truncate text-label font-bold text-text">{file.name}</span>
            <span className="tabular shrink-0 text-data-sm text-subtext0">
              {bytes(file.size_bytes)} · {file.shards.length}{' '}
              {file.shards.length === 1 ? 'shard' : 'shards'}
            </span>
          </div>

          {/* Proportional to real shard sizes, with a hairline crust gap at
              each boundary so the segmentation reads as structure. */}
          <div className="mb-3 flex h-3 w-full gap-px overflow-hidden bg-crust">
            {file.shards.map((shard) => {
              const share =
                file.size_bytes > 0 ? (shard.plain_size_bytes / file.size_bytes) * 100 : 100;
              return (
                <div
                  key={shard.index}
                  className={clsx(
                    bgFor(shard.account_id),
                    activeShard === shard.index && 'shard-active',
                  )}
                  style={{ width: `${Math.max(share, 1)}%` }}
                />
              );
            })}
          </div>

          <ul className="mb-3 space-y-1.5">
            {file.shards.map((shard) => {
              const missing = shardColor(shard.account_id, drives) === null;
              return (
                <li key={shard.index} className="flex items-center gap-2 text-data">
                  <span className={clsx('h-2 w-2 shrink-0', bgFor(shard.account_id))} />
                  <span className={clsx('flex-1 truncate', textFor(shard.account_id))}>
                    {driveLabel(shard.account_id)}
                  </span>
                  <span className="tabular shrink-0 text-subtext0">shard {shard.index}</span>
                  <span className="tabular w-20 shrink-0 text-right text-subtext0">
                    {bytes(shard.plain_size_bytes)}
                  </span>
                  {/* Never colour alone: the glyph carries the state too. */}
                  <span
                    className={clsx('w-4 shrink-0 text-center', missing ? 'text-red' : 'text-green')}
                    title={missing ? 'Drive disconnected' : 'Recorded'}
                  >
                    {missing ? '✕' : '✓'}
                  </span>
                </li>
              );
            })}
          </ul>

          <div className="flex items-center justify-between border-t border-surface0 pt-2">
            <span className="tabular truncate text-data-sm text-subtext0">
              {file.sha256 ? `sha256 ${file.sha256.slice(0, 4)}…${file.sha256.slice(-4)}` : ''}
            </span>
            <span className="flex items-center gap-1 text-data-sm text-subtext0">
              {file.is_encrypted ? (
                <>
                  AES-256-GCM <Lock size={12} aria-hidden />
                </>
              ) : (
                <span className="text-yellow">not encrypted</span>
              )}
            </span>
          </div>
        </div>
      )}
    </div>
  );
}
