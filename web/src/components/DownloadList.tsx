import { useDownloads } from '../lib/downloads-context';

/**
 * Downloads in progress, rendered wherever the user happens to be — same
 * placement rationale as UploadList.
 *
 * There is no progress bar and no "done" state here on purpose: see
 * downloads.ts for why nothing in this process can observe either
 * truthfully once the browser has taken the transfer. This is a "something
 * is happening" indicator, dismissed by the user once they see it land.
 */
export function DownloadList() {
  const { jobs, dismiss } = useDownloads();
  if (jobs.length === 0) return null;

  return (
    <section className="card mb-6 p-4" aria-label="Downloads">
      <h2 className="mb-3 text-label font-semibold text-text">Downloads</h2>
      <ul className="space-y-2">
        {jobs.map((job) => (
          <li key={job.id} className="flex items-center justify-between gap-3">
            <span className="truncate text-body text-text">{job.name}</span>
            <span
              className={
                job.status === 'error'
                  ? 'shrink-0 text-data-sm text-danger'
                  : 'shrink-0 text-data-sm text-muted'
              }
            >
              {job.status === 'error' ? (job.error ?? 'Could not start.') : 'Downloading…'}
            </span>
            <button
              type="button"
              className="shrink-0 rounded px-2 py-1 text-caption text-muted
                         transition-colors duration-hover hover:bg-raised hover:text-text"
              onClick={() => dismiss(job.id)}
            >
              Dismiss
            </button>
          </li>
        ))}
      </ul>
    </section>
  );
}
