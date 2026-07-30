import clsx from 'clsx';

import { bytes } from '../lib/format';
import { useUploads } from '../lib/uploads-context';
import { isActive, type UploadJob } from '../lib/uploads';

/**
 * The state of every upload, rendered wherever the user happens to be.
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
  const { jobs, cancel, dismiss } = useUploads();
  if (jobs.length === 0) return null;

  return (
    <ul className="mb-4 space-y-2">
      {jobs.map((job) => {
        const live = isActive(job);
        const fraction = job.size > 0 ? Math.min(1, job.sent / job.size) : 0;
        return (
          <li key={job.id} className="card px-2ch py-1line">
            <div className="mb-1.5 flex items-baseline justify-between gap-3">
              <span className="truncate text-label text-text">{job.name}</span>
              <span
                className={clsx(
                  'tabular shrink-0 text-data-sm',
                  job.status === 'error' ? 'text-red' : 'text-subtext0',
                )}
              >
                {label(job)}
              </span>
            </div>
            <div className="h-1 w-full overflow-hidden bg-surface0">
              <div
                className={clsx(
                  'h-full transition-[width] duration-hover',
                  job.status === 'error' && 'bg-red',
                  job.status === 'cancelled' && 'bg-overlay0',
                  job.status === 'done' && 'bg-green',
                  live && 'bg-mauve',
                )}
                style={{ width: `${(job.status === 'done' ? 1 : fraction) * 100}%` }}
              />
            </div>
            <div className="mt-1.5 text-right">
              <button
                type="button"
                className="text-caption text-subtext0 hover:text-red"
                onClick={() => (live ? cancel(job.id) : dismiss(job.id))}
              >
                {live ? 'Cancel' : 'Dismiss'}
              </button>
            </div>
          </li>
        );
      })}
    </ul>
  );
}
