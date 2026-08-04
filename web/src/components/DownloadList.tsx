import { useDownloads } from '../lib/downloads-context';

/**
 * Downloads handed to the browser, rendered wherever the user happens to be —
 * same placement rationale as UploadList.
 *
 * DELIBERATELY NOT A PROGRESS UI. `a.click()` hands the transfer entirely to
 * the browser/webview and JS is never told anything more about it, which is
 * precisely what keeps downloads off the heap (known issue #15). There is no
 * byte count to render, so there is no bar, no spinner, no percentage and no
 * indeterminate sweep — any of those would imply this row is tracking
 * something it cannot see.
 *
 * The label says what is actually true: the transfer was handed over. Not
 * "Downloading…", which reads as live tracking of something in flight.
 *
 * Real byte-level progress needs the transfer to run through Go rather than
 * the webview, which is a desktop-only path with a native save picker. That is
 * Block 6; `files.ProgressReader` (known issue #40) is already built for it
 * and is waiting on exactly that route.
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
              {job.status === 'error'
                ? (job.error ?? 'Could not start that download.')
                : 'Handed to your browser'}
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
