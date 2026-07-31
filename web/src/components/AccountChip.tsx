import clsx from 'clsx';

import { DRIVE_BG, UNKNOWN_DRIVE_BG, driveColor } from '../lib/format';

/**
 * An account's identity mark: its number, on its colour.
 *
 * Design.md §5, "addresses, not fragments". **The number is the identity and
 * the colour is reinforcement — not the other way round.** That ordering was
 * forced by measurement, not taste (known issue #29):
 *
 *   - Six is the most colours that stay >= 20 dE2000 clear of success green,
 *     warning amber and error red while remaining calm and legible on the dark
 *     surfaces. Eight crowds to 10.5 dE, and the old ramp had two accounts
 *     *byte-identical* to semantic colours.
 *   - No hue-only ramp survives colour-vision deficiency: the best achievable
 *     eight-colour set scores 8.0 dE under dichromacy, below the ~10 at which
 *     small marks are distinguishable. A digit has no such failure mode.
 *
 * So a seventh account reusing colour 1 is harmless: the numbers differ, and
 * the number is what is being read. This is also what makes a row of shards
 * read as an ordered inventory rather than as confetti.
 */
export function AccountChip({
  ordinal,
  size = 'md',
  className,
}: {
  /** The account's persisted `ordinal`, or null when it cannot be resolved. */
  ordinal: number | null;
  size?: 'sm' | 'md';
  className?: string;
}) {
  const known = ordinal !== null;
  return (
    <span
      // `rounded` rather than a literal: it is 0 under the current tokens and
      // picks up the modest radius when the token set lands, with no edit here.
      className={clsx(
        'inline-flex shrink-0 items-center justify-center rounded font-semibold tabular-nums',
        // Dark text on a light chip. Every ramp colour clears 4.5:1 against
        // --crust by construction, so this is legible on all six.
        known ? 'text-canvas' : 'text-muted',
        known ? DRIVE_BG[driveColor(ordinal)] : UNKNOWN_DRIVE_BG,
        size === 'sm' ? 'h-3.5 w-3.5 text-[9px]' : 'h-[18px] w-[18px] text-[11px]',
        className,
      )}
      aria-hidden
    >
      {/* Never colour alone, and never a bare colour swatch either: an
          unidentifiable shard says so rather than showing a number it does
          not have. */}
      {known ? ordinal : '?'}
    </span>
  );
}
