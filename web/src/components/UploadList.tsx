import clsx from 'clsx';

import { bytes } from '../lib/format';
import { useUploads } from '../lib/uploads-context';
import { isActive, type UploadJob } from '../lib/uploads';

/**
 * Transfers in progress, rendered wherever the user happens to be.
 *
 * Mounted in Layout rather than on the Files page, because an upload started
 * on Files and then navigated away from used to vanish from the UI while it
 * kept streaming — known issue #13.
 */
function label(job: UploadJob): string {
  switch (job.status) {
    case 'sending':
      return `${bytes(job.sent)} of ${bytes(job.size)}`;
    case 'finishing':
      // Not "100%". The browser has sent everything; the server is still
      // writing the last shards to a drive, and claiming completion here is
      // the kind of small lie that makes a UI untrustworthy.
      return 'Finishing on the server…';
    case 'done':
      return 'Uploaded';
    case 'cancelled':
      return 'Cancelled';
    case 'error':
      return job.error ?? 'Upload failed.';
  }
}

export function UploadList() {
  const { jobs, cancel, dismiss, clearSettled, settledCount } = useUploads();
  if (jobs.length === 0) return null;

  const live = jobs.filter(isActive).length;

  return (
    <section className="card mb-6 p-4" aria-label="Transfers">
      <div className="mb-3 flex items-baseline justify-between gap-3">
        <h2 className="text-label font-semibold text-text">Transfers</h2>
        <div className="flex items-baseline gap-3">
          {live > 0 && (
            <span className="tabular text-data-sm text-muted">
              {live} in progress
            </span>
          )}
          {settledCount > 0 && (
            <button
              type="button"
              className="rounded px-2 py-1 text-caption text-muted
                         transition-colors duration-hover hover:bg-raised hover:text-text"
              onClick={clearSettled}
              // Named rather than a bare "Clear" so it is obvious that a
              // running upload is not about to be cancelled.
              aria-label={`Clear ${settledCount} finished ${settledCount === 1 ? 'transfer' : 'transfers'}`}
            >
              Clear finished
            </button>
          )}
        </div>
      </div>

      <ul className="space-y-3">
        {jobs.map((job) => {
          const active = isActive(job);
          const fraction = job.size > 0 ? Math.min(1, job.sent / job.size) : 0;
          // The bar must not claim completion the server has not reported.
          // `finishing` means every byte has left this machine and the server
          // is still writing shards to a drive, so its duration is unknown —
          // an indeterminate sweep, not a full bar.
          const indeterminate = job.status === 'finishing';

          return (
            <li key={job.id}>
              <div className="mb-1.5 flex items-baseline justify-between gap-3">
                <span className="truncate text-body text-text">{job.name}</span>
                <span
                  className={clsx(
                    'tabular shrink-0 text-data-sm',
                    job.status === 'error' ? 'text-danger' : 'text-muted',
                  )}
                >
                  {label(job)}
                </span>
              </div>

              <div className="flex items-center gap-3">
                <div className="h-1.5 flex-1 overflow-hidden rounded-full bg-raised">
                  {indeterminate ? (
                    // Under prefers-reduced-motion the sweep does not run, so
                    // the track shows a static partial bar and the label
                    // carries the meaning. Never a full bar.
                    <div className="h-full w-1/4 rounded-full bg-accent motion-safe:animate-sweep" />
                  ) : (
                    <div
                      className={clsx(
                        'h-full rounded-full transition-[width] duration-hover',
                        job.status === 'error' && 'bg-danger',
                        job.status === 'cancelled' && 'bg-borderStrong',
                        job.status === 'done' && 'bg-success',
                        job.status === 'sending' && 'bg-accent',
                      )}
                      style={{ width: `${(job.status === 'done' ? 1 : fraction) * 100}%` }}
                    />
                  )}
                </div>

                <button
                  type="button"
                  className="shrink-0 rounded px-2 py-1 text-caption text-muted
                             transition-colors duration-hover hover:bg-raised hover:text-text"
                  onClick={() => (active ? cancel(job.id) : dismiss(job.id))}
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
