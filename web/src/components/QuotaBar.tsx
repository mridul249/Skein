import clsx from 'clsx';

import type { Drive, Quota } from '../lib/api';
import { DRIVE_BG, bytes, driveColor, percent, usageTone } from '../lib/format';
import { Overlay } from './Overlay';
import { AccountChip } from './AccountChip';

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
      <div className="card px-2ch py-1line">
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
  /**
   * Off for the sidebar rail. That copy is decorative: the per-drive rows
   * directly beneath it state the same numbers as text, so exposing it too
   * would announce the pool twice and put a keyboard stop on every drive for
   * information the user is about to read anyway.
   */
  showTooltips?: boolean;
}) {
  const totalUsed = drives.reduce((sum, d) => sum + d.used_bytes, 0);
  const freeShare = total > 0 ? Math.max(0, ((total - totalUsed) / total) * 100) : 100;
  const decorative = !showTooltips;

  return (
    <div
      className={clsx('flex w-full overflow-hidden bg-surface0', height)}
      {...(decorative
        ? { 'aria-hidden': true }
        : {
            role: 'group',
            'aria-label': `Pooled storage: ${bytes(totalUsed)} used of ${bytes(total)}`,
          })}
    >
      {drives.map((drive) => {
        const share = usedShare(drive, total);
        const free = Math.max(0, drive.total_bytes - drive.used_bytes);
        const label = `${drive.email}: ${bytes(drive.used_bytes)} of ${bytes(
          drive.total_bytes,
        )} used, ${bytes(free)} free`;

        return (
          <div
            key={drive.id}
            className="h-full motion-safe:transition-[width] motion-safe:duration-300"
            style={{
              width: `${share}%`,
              // A drive holding a few megabytes of a 400 GB pool rounds to a
              // sub-pixel slice. The floor keeps it visible and hoverable —
              // an account you cannot see is an account you cannot inspect.
              minWidth: share > 0 ? '3px' : undefined,
              // Used segments never give up their floor. See the remainder
              // below for what absorbs the resulting overflow.
              flexShrink: 0,
            }}
          >
            <Overlay
              disabled={!showTooltips}
              label={label}
              className={clsx(
                'block h-full w-full cursor-default border-r border-crust',
                'hover:brightness-125 focus-visible:brightness-125',
                DRIVE_BG[driveColor(drive.ordinal)],
              )}
              content={
                <div className="space-y-0.5">
                  <div className="text-label font-bold text-text">{drive.email}</div>
                  <div className="tabular text-data-sm text-subtext0">
                    {bytes(drive.used_bytes)} of {bytes(drive.total_bytes)} ·{' '}
                    {percent(drive.used_bytes, drive.total_bytes)}%
                  </div>
                  <div className="tabular text-data-sm text-subtext0">{bytes(free)} free</div>
                </div>
              }
            />
          </div>
        );
      })}

      {/*
        One remainder block, always last — and the thing that absorbs overflow.

        Segment widths sum to exactly 100%, but each used segment also carries
        a 3px floor. Whenever a floor exceeds a segment's true share the row
        adds up to more than the container, and the default flex behaviour is
        to shrink *every* item proportionally — which silently distorts each
        drive's share to pay for another drive's floor.

        So the remainder shrinks and the used segments do not: it is the only
        item with a shrink factor. Free space is the honest place to take it
        from, because the coloured run is the answer to "how much have I used"
        and must stay true; the remainder is already whatever is left over.
      */}
      <div
        className="h-full bg-surface0 motion-safe:transition-[width] motion-safe:duration-300"
        style={{ width: `${freeShare}%`, flexShrink: 1, minWidth: 0 }}
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
    <div className="space-y-3 border-t border-surface0 px-2ch py-1line">
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
                <AccountChip ordinal={drive.ordinal} size="sm" />
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
