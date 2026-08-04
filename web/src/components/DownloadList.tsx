import { useDownloads } from '../lib/downloads-context';
import { downloadDestination } from '../lib/platform';

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
 * The label says what is actually true for the shell in use: the browser build
 * hands the transfer to the browser, and the desktop build's webview writes it
 * to the Downloads folder. Not "Downloading…", which reads as live tracking of
 * something in flight, and not "handed to your browser" on desktop, where
 * there is no browser and the file has already landed.
 *
 * Real byte-level progress needs the transfer to run through Go rather than
 * the webview, which is a desktop-only path with a native save picker. That is
 * Block 6; `files.ProgressReader` (known issue #40) is already built for it
 * and is waiting on exactly that route.
 */
export function DownloadList() {
  const { jobs, dismiss } = useDownloads();
  if (jobs.length === 0) return null;

  // On desktop the webview writes straight to the Downloads folder and there
  // is no browser download manager to look in, so "handed to your browser"
  // is simply false there — the file is already on disk.
  const destination = downloadDestination();

  return (
    <section className="card mb-6 p-4" aria-label="Downloads">
      <h2 className="mb-3 text-label font-semibold text-text">Downloads</h2>
      {/*
        A grid, not flex with justify-between.

        With three flex children the status text floats to wherever the
        filename happens to end, so it lands in a different place on every row
        and the column reads as ragged. Fixed columns line the status and the
        action up down the list; only the name column flexes, and it truncates.
      */}
      <ul className="space-y-1">
        {jobs.map((job) => (
          <li
            key={job.id}
            className="grid grid-cols-[minmax(0,1fr)_auto_auto] items-baseline gap-x-4"
          >
            <span className="truncate text-body text-text" title={job.name}>
              {job.name}
            </span>
            <span
              className={
                job.status === 'error'
                  ? 'text-data-sm text-danger'
                  : 'text-data-sm text-muted'
              }
            >
              {job.status === 'error'
                ? (job.error ?? 'Could not start that download.')
                : destination}
            </span>
            <button
              type="button"
              className="justify-self-end rounded px-2 py-1 text-caption text-muted
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
