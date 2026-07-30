import clsx from 'clsx';

import type { Drive, Quota } from '../lib/api';
import { DRIVE_BG, bytes, driveColor, percent, usageTone } from '../lib/format';
import { Tooltip } from './Tooltip';

/**
 * The pooled quota bar. Design.md §4.
 *
 * Used bytes are packed left, drive by drive in connection order, followed by
 * one grey remainder for everything still free across the pool:
 *
 *     [used₁][used₂][──────── free ────────]
 *
 * The earlier version sized each drive's slot by its share of the pool and
 * filled that slot by its own usage, which renders as [used₁][free₁][used₂]
 * [free₂] — alternating light and dark bands that read as stripes rather than
 * as one pool. Packing left means the coloured run is the answer to "how much
 * have I used", which is the question the bar exists to answer.
 */

function usedShare(drive: Drive, total: number): number {
  if (total <= 0) return 0;
  return (drive.used_bytes / total) * 100;
}

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
      <div className="min-w-[12rem] flex-1">
        <PackedBar drives={drives} total={quota.total_bytes} height="h-2.5" />
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
 * The bar itself, shared by the page header and the sidebar rail so the two
 * can never drift apart.
 */
function PackedBar({
  drives,
  total,
  height,
  showTooltips = true,
}: {
  drives: Drive[];
  total: number;
  height: string;
  showTooltips?: boolean;
}) {
  const totalUsed = drives.reduce((sum, d) => sum + d.used_bytes, 0);
  const freeShare = total > 0 ? Math.max(0, ((total - totalUsed) / total) * 100) : 100;

  return (
    <div
      className={clsx('flex w-full overflow-hidden rounded-full bg-surface0', height)}
      role="group"
      aria-label={`Pooled storage: ${bytes(totalUsed)} used of ${bytes(total)}`}
    >
      {drives.map((drive) => {
        const share = usedShare(drive, total);
        const label = `${drive.email}: ${bytes(drive.used_bytes)} of ${bytes(
          drive.total_bytes,
        )} used, ${bytes(Math.max(0, drive.total_bytes - drive.used_bytes))} free`;

        return (
          <div
            key={drive.id}
            // Lift the width styles and transition to this explicit flex child
            className="h-full motion-safe:transition-[width] motion-safe:duration-300"
            style={{
              width: `${share}%`,
              minWidth: share > 0 ? '3px' : undefined,
            }}
          >
            <Tooltip
              disabled={!showTooltips}
              // Tell the tooltip wrapper to take up the full space
              className="h-full w-full block"
              content={
                <div className="space-y-0.5">
                  <div className="text-label font-medium text-text">{drive.email}</div>
                  <div className="tabular text-data-sm text-subtext0">
                    {bytes(drive.used_bytes)} of {bytes(drive.total_bytes)} ·{' '}
                    {percent(drive.used_bytes, drive.total_bytes)}%
                  </div>
                  <div className="tabular text-data-sm text-overlay0">
                    {bytes(Math.max(0, drive.total_bytes - drive.used_bytes))} free
                  </div>
                </div>
              }
            >
              <div
                tabIndex={showTooltips ? 0 : -1}
                role="img"
                aria-label={label}
                title={showTooltips ? undefined : label}
                // Change width handling here: remove the `style` prop and add `w-full`
                className={clsx(
                  'h-full w-full cursor-default border-r border-crust',
                  'hover:brightness-125 focus-visible:brightness-125',
                  DRIVE_BG[driveColor(drive.ordinal)],
                )}
              />
            </Tooltip>
          </div>
        );
      })}

      {/* One remainder block, always last. */}
      <div
        className="h-full bg-surface0 motion-safe:transition-[width] motion-safe:duration-300"
        style={{ width: `${freeShare}%` }}
      />
    </div>
  );
}

/**
 * The sidebar rail. Design.md §4: permanently visible, one row per drive,
 * because capacity is the thing that runs out.
 */
export function QuotaRail({ quota }: { quota: Quota | undefined }) {
  if (!quota || quota.drives.length === 0) return null;

  const active = quota.drives.filter((d) => d.status !== 'disabled');

  return (
    <div className="space-y-3 border-t border-surface0 px-4 py-4">
      {active.length > 0 && (
        // The same packed bar, compact, tooltips off — the per-drive rows
        // below already carry the numbers.
        <PackedBar drives={active} total={quota.total_bytes} height="h-1.5" showTooltips={false} />
      )}

      {quota.drives.map((drive) => {
        const pct = percent(drive.used_bytes, drive.total_bytes);
        const tone = usageTone(drive.used_bytes, drive.total_bytes);
        return (
          <div key={drive.id} className="space-y-1">
            <div className="flex items-baseline justify-between gap-2">
              <span className="flex min-w-0 items-center gap-1.5">
                <span
                  className={clsx(
                    'h-2 w-2 shrink-0 rounded-full',
                    DRIVE_BG[driveColor(drive.ordinal)],
                  )}
                  aria-hidden
                />
                <span className="truncate text-data-sm text-subtext0" title={drive.email}>
                  {drive.email}
                </span>
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
