import clsx from 'clsx';

import type { Quota } from '../lib/api';
import { DRIVE_BG, bytes, driveColor, percent, usageTone } from '../lib/format';

/**
 * The pooled quota bar. Design.md §4: it sits directly under the page title,
 * segmented by drive in each drive's colour, and it is the first thing you
 * see — because capacity is the constraint this tool exists to manage.
 */
export function QuotaBar({ quota }: { quota: Quota }) {
  const drives = quota.drives.filter((d) => d.status !== 'disabled');

  if (drives.length === 0) {
    return (
      <div className="card px-4 py-3">
        <p className="text-body text-subtext0">
          No drives connected.{' '}
          <a href="/settings" className="text-sapphire underline underline-offset-2">
            Connect one
          </a>{' '}
          to start storing files.
        </p>
      </div>
    );
  }

  return (
    <div className="flex flex-wrap items-center gap-4">
      <div className="flex h-3 min-w-[12rem] flex-1 gap-px overflow-hidden rounded bg-mantle">
        {drives.map((drive) => {
          // Each drive's segment is sized by its share of the pool, and
          // filled by how much of itself is used.
          const share = quota.total_bytes > 0 ? (drive.total_bytes / quota.total_bytes) * 100 : 0;
          return (
            <div
              key={drive.id}
              className="relative h-full bg-surface0"
              style={{ width: `${share}%` }}
              title={`${drive.email}: ${bytes(drive.used_bytes)} of ${bytes(drive.total_bytes)}`}
            >
              <div
                className={clsx('h-full', DRIVE_BG[driveColor(drive.ordinal)])}
                style={{ width: `${percent(drive.used_bytes, drive.total_bytes)}%` }}
              />
            </div>
          );
        })}
      </div>

      <p className="tabular shrink-0 text-data text-subtext0">
        {bytes(quota.used_bytes)} / {bytes(quota.total_bytes)}
        <span className="mx-2 text-overlay0">·</span>
        {drives.length} {drives.length === 1 ? 'drive' : 'drives'}
      </p>
    </div>
  );
}

/**
 * The sidebar rail. Design.md §4: permanently visible, one row per drive,
 * because capacity is the thing that runs out.
 */
export function QuotaRail({ quota }: { quota: Quota | undefined }) {
  if (!quota || quota.drives.length === 0) return null;

  return (
    <div className="space-y-2 border-t border-surface0 px-4 py-4">
      {quota.drives.map((drive) => {
        const pct = percent(drive.used_bytes, drive.total_bytes);
        const tone = usageTone(drive.used_bytes, drive.total_bytes);
        return (
          <div key={drive.id} className="space-y-1">
            <div className="flex items-baseline justify-between gap-2">
              <span className="truncate text-data-sm text-subtext0" title={drive.email}>
                {drive.email}
              </span>
              <span
                className={clsx(
                  'tabular shrink-0 text-data-sm',
                  tone === 'red' && 'text-red',
                  tone === 'yellow' && 'text-yellow',
                  tone === 'green' && 'text-subtext0',
                )}
              >
                {pct}%
              </span>
            </div>
            <div className="h-1 w-full overflow-hidden rounded bg-surface0">
              <div
                className={clsx('h-full', DRIVE_BG[driveColor(drive.ordinal)])}
                style={{ width: `${pct}%` }}
              />
            </div>
            {drive.status !== 'active' && (
              <p className="text-data-sm text-yellow">
                {drive.status === 'needs_reauth' ? 'Reconnect this drive' : drive.status}
              </p>
            )}
          </div>
        );
      })}
    </div>
  );
}
