import clsx from 'clsx';

import { bytes } from '../lib/format';
import {
  formatEta,
  isTerminal,
  type DesktopDownloadJob,
} from '../lib/desktop-downloads';

/**
 * The desktop download drawer, with REAL bytes.
 *
 * Unlike DownloadList — which deliberately shows no progress because the
 * browser owns those transfers and tells JS nothing (#15) — every number here
 * is measured server-side by files.ProgressReader as the bytes stream through
 * Go to disk. A bar is honest here precisely because it is fed by a byte
 * count rather than by a guess.
 */
export function DesktopDownloadDrawer({
  jobs,
  onCancel,
  onDismiss,
  onClear,
}: {
  jobs: DesktopDownloadJob[];
  onCancel: (id: string) => void;
  onDismiss: (id: string) => void;
  onClear: () => void;
}) {
  if (jobs.length === 0) return null;

  const settled = jobs.filter(isTerminal).length;
  const running = jobs.length - settled;

  return (
    <section className="card mb-6 p-4" aria-label="Downloads">
      <div className="mb-3 flex items-baseline justify-between gap-3">
        <h2 className="text-label font-semibold text-text">Downloads</h2>
        <div className="flex items-baseline gap-3">
          {running > 0 && (
            <span className="tabular text-data-sm text-muted">{running} in progress</span>
          )}
          {settled > 0 && (
            <button
              type="button"
              onClick={onClear}
              aria-label={`Clear ${settled} finished ${settled === 1 ? 'download' : 'downloads'}`}
              className="rounded px-2 py-1 text-caption text-muted
                         transition-colors duration-hover hover:bg-raised hover:text-text"
            >
              Clear finished
            </button>
          )}
        </div>
      </div>

      <ul className="space-y-3">
        {jobs.map((job) => {
          const active = !isTerminal(job);
          const fraction = job.total > 0 ? Math.min(1, job.done / job.total) : 0;
          const eta = formatEta(job.eta_seconds);

          return (
            <li key={job.id}>
              <div className="mb-1.5 grid grid-cols-[minmax(0,1fr)_auto] items-baseline gap-x-4">
                <span className="truncate text-body text-text" title={job.path}>
                  {job.name}
                </span>
                <span
                  className={clsx(
                    'tabular text-data-sm',
                    job.state === 'failed' ? 'text-danger' : 'text-muted',
                  )}
                >
                  {label(job, eta)}
                </span>
              </div>

              <div className="flex items-center gap-3">
                <div className="h-1.5 flex-1 overflow-hidden rounded-full bg-raised">
                  <div
                    className={clsx(
                      'h-full rounded-full transition-[width] duration-hover',
                      job.state === 'failed' && 'bg-danger',
                      job.state === 'cancelled' && 'bg-borderStrong',
                      job.state === 'complete' && 'bg-success',
                      job.state === 'running' && 'bg-accent',
                    )}
                    style={{ width: `${(job.state === 'complete' ? 1 : fraction) * 100}%` }}
                  />
                </div>

                <button
                  type="button"
                  className="shrink-0 rounded px-2 py-1 text-caption text-muted
                             transition-colors duration-hover hover:bg-raised hover:text-text"
                  onClick={() => (active ? onCancel(job.id) : onDismiss(job.id))}
                >
                  {active ? 'Cancel' : 'Dismiss'}
                </button>
              </div>
            </li>
          );
        })}
      </ul>
    </section>
  );
}

function label(job: DesktopDownloadJob, eta: string): string {
  switch (job.state) {
    case 'running': {
      const rate = job.bytes_per_sec > 0 ? ` · ${bytes(job.bytes_per_sec)}/s` : '';
      // eta is '' when unknown — the trailing separator goes with it, so the
      // line never reads "… · " with nothing after it.
      const remaining = eta ? ` · ${eta} left` : '';
      return `${bytes(job.done)} of ${bytes(job.total)}${rate}${remaining}`;
    }
    case 'complete':
      return 'Saved';
    case 'cancelled':
      return 'Cancelled';
    case 'failed':
      return job.error ?? 'Download failed.';
  }
}
