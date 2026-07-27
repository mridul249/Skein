/** Formatting helpers. Design.md §7: plain, specific, never apologetic. */

const UNITS = ['B', 'KB', 'MB', 'GB', 'TB', 'PB'] as const;

/** bytes formats a size the way the file table shows it. */
export function bytes(n: number): string {
  if (!Number.isFinite(n) || n < 0) return '—';
  if (n < 1024) return `${n} B`;

  let value = n;
  let unit = 0;
  while (value >= 1024 && unit < UNITS.length - 1) {
    value /= 1024;
    unit++;
  }
  // One decimal below 10, none above: 9.4 GB reads better than 9 GB, and
  // 384.2 GB is noise.
  return `${value < 10 ? value.toFixed(1) : Math.round(value)} ${UNITS[unit]}`;
}

/** relativeTime renders "2d ago" rather than a timestamp nobody parses. */
export function relativeTime(iso: string): string {
  const then = new Date(iso).getTime();
  if (Number.isNaN(then)) return '—';

  const seconds = Math.max(0, Math.floor((Date.now() - then) / 1000));
  if (seconds < 60) return 'just now';

  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m ago`;

  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours}h ago`;

  const days = Math.floor(hours / 24);
  if (days < 7) return `${days}d ago`;

  const weeks = Math.floor(days / 7);
  if (weeks < 5) return `${weeks}w ago`;

  const months = Math.floor(days / 30);
  if (months < 12) return `${months}mo ago`;

  return `${Math.floor(days / 365)}y ago`;
}

/**
 * driveColor maps a drive's connection ordinal to its colour.
 *
 * Design.md §5: the colour *is* the drive's identity, everywhere. The same
 * drive is the same colour in the sidebar rail, the pooled quota bar and every
 * shard dot, and that consistency is what turns a palette into an information
 * system.
 */
const RAMP = [
  'drive1',
  'drive2',
  'drive3',
  'drive4',
  'drive5',
  'drive6',
  'drive7',
  'drive8',
] as const;

export type DriveColor = (typeof RAMP)[number];

export function driveColor(ordinal: number): DriveColor {
  const index = ((ordinal - 1) % RAMP.length + RAMP.length) % RAMP.length;
  return RAMP[index] ?? 'drive1';
}

/** Tailwind cannot see a class built at runtime, so the pairs are literal. */
export const DRIVE_BG: Record<DriveColor, string> = {
  drive1: 'bg-drive1',
  drive2: 'bg-drive2',
  drive3: 'bg-drive3',
  drive4: 'bg-drive4',
  drive5: 'bg-drive5',
  drive6: 'bg-drive6',
  drive7: 'bg-drive7',
  drive8: 'bg-drive8',
};

export const DRIVE_TEXT: Record<DriveColor, string> = {
  drive1: 'text-drive1',
  drive2: 'text-drive2',
  drive3: 'text-drive3',
  drive4: 'text-drive4',
  drive5: 'text-drive5',
  drive6: 'text-drive6',
  drive7: 'text-drive7',
  drive8: 'text-drive8',
};

/** usageTone colours a quota bar by how full it is. */
export function usageTone(used: number, total: number): 'green' | 'yellow' | 'red' {
  if (total <= 0) return 'green';
  const ratio = used / total;
  if (ratio >= 0.95) return 'red';
  if (ratio >= 0.8) return 'yellow';
  return 'green';
}

export function percent(used: number, total: number): number {
  if (total <= 0) return 0;
  return Math.min(100, Math.max(0, Math.round((used / total) * 100)));
}
