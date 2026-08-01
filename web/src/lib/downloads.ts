/**
 * Tracks that a download was started, so the desktop build has somewhere to
 * show it. Deliberately not a progress bar: an `<a download>` click hands
 * the transfer entirely to the browser/webview, and JS has no visibility
 * into it from that point on — that absence of visibility is what keeps the
 * download off-heap in the first place (known issue #15). Faking a
 * completion signal would be the same small lie Design.md already rules out
 * for the upload `finishing` state.
 *
 * A job here has exactly two states: `downloading`, until the user
 * dismisses it themselves, or `error`, if the request that mints the
 * capability URL fails before any bytes could move. There is no `done`,
 * because nothing in this process can observe that truthfully.
 */

export type DownloadStatus = 'downloading' | 'error';

export interface DownloadJob {
  id: string;
  name: string;
  status: DownloadStatus;
  error?: string;
}

export type SettledListener = (job: DownloadJob) => void;

export class DownloadStore {
  private jobs: DownloadJob[] = [];
  private seq = 0;
  private listeners = new Set<() => void>();

  constructor(private readonly onSettled?: SettledListener) {}

  subscribe = (fn: () => void): (() => void) => {
    this.listeners.add(fn);
    return () => this.listeners.delete(fn);
  };

  getSnapshot = (): DownloadJob[] => this.jobs;

  private notify(): void {
    for (const fn of this.listeners) fn();
  }

  private patch(id: string, fn: (job: DownloadJob) => DownloadJob): void {
    this.jobs = this.jobs.map((j) => (j.id === id ? fn(j) : j));
    this.notify();
  }

  start(name: string): string {
    const id = `download-${++this.seq}`;
    this.jobs = [...this.jobs, { id, name, status: 'downloading' }];
    this.notify();
    return id;
  }

  fail(id: string, error: string): void {
    this.patch(id, (job) => ({ ...job, status: 'error', error }));
    const job = this.jobs.find((j) => j.id === id);
    if (job) this.onSettled?.(job);
  }

  dismiss(id: string): void {
    this.jobs = this.jobs.filter((j) => j.id !== id);
    this.notify();
  }
}
