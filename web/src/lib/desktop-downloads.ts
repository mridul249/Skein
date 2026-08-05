/**
 * Client for the Go-side download path.
 *
 * Real bytes, unlike the browser path: the server streams the file through Go
 * to disk, so it can report progress, cancel for real, and surface an error
 * mid-transfer. See internal/files/desktopdownload.go.
 */
import { probeDesktop } from './desktop';
import { accessTokenForStreamURL, authedFetch } from './api';

export type DesktopDownloadState = 'running' | 'complete' | 'failed' | 'cancelled';

export interface DesktopDownloadJob {
  id: string;
  file_id: string;
  name: string;
  path: string;
  state: DesktopDownloadState;
  done: number;
  total: number;
  bytes_per_sec: number;
  /** -1 means unknown. The UI renders that BLANK, never "0s". */
  eta_seconds: number;
  error?: string;
}

export function isTerminal(job: DesktopDownloadJob): boolean {
  return job.state !== 'running';
}

/** Whether the Go-side path is available in this build. */
export async function desktopDownloadsAvailable(): Promise<boolean> {
  return (await probeDesktop()).desktopDownloads;
}

/**
 * DesktopDownloadStore tracks Go-side transfers.
 *
 * Lives above the router like the upload store, for the same reason: a
 * download started on Files and navigated away from must not lose its handle.
 */
export class DesktopDownloadStore {
  private jobs: DesktopDownloadJob[] = [];
  private readonly listeners = new Set<() => void>();
  private readonly streams = new Map<string, () => void>();

  subscribe = (fn: () => void): (() => void) => {
    this.listeners.add(fn);
    return () => {
      this.listeners.delete(fn);
    };
  };

  getSnapshot = (): DesktopDownloadJob[] => this.jobs;

  private emit(): void {
    for (const fn of this.listeners) fn();
  }

  private upsert(job: DesktopDownloadJob): void {
    const i = this.jobs.findIndex((j) => j.id === job.id);
    this.jobs = i < 0 ? [...this.jobs, job] : this.jobs.map((j) => (j.id === job.id ? job : j));
    this.emit();
  }

  /** Adds a job and begins watching its event stream. */
  track(job: DesktopDownloadJob): void {
    this.upsert(job);
    this.watch(job.id);
  }

  /**
   * watch attaches an EventSource to a download.
   *
   * RECONNECT: the transfer is owned by the server, never by this connection.
   * If the stream drops the download keeps going, and re-attaching is just
   * subscribing again — the server sends the CURRENT snapshot first, so a
   * client that missed samples is immediately correct rather than replaying.
   */
  private watch(id: string): void {
    this.streams.get(id)?.();
    void this.openStream(id);
  }

  /**
   * THE TOKEN GOES IN THE QUERY STRING because EventSource cannot set headers.
   * StreamAuth accepts it there for GET-only streaming routes; see
   * internal/httpapi/middleware/streamauth.go for why that is bounded.
   */
  private async openStream(id: string): Promise<void> {
    const token = await accessTokenForStreamURL();
    if (!token) return;

    // A dismiss may have landed while the token was being fetched; do not
    // open a stream for a job that is no longer on the list.
    if (!this.jobs.some((j) => j.id === id)) return;

    const source = new EventSource(
      `/api/desktop/downloads/${id}/events?access_token=${encodeURIComponent(token)}`,
    );
    let closed = false;

    const stop = () => {
      if (closed) return;
      closed = true;
      source.close();
      this.streams.delete(id);
    };
    this.streams.set(id, stop);

    source.addEventListener('progress', (e) => {
      try {
        const job = JSON.parse((e as MessageEvent<string>).data) as DesktopDownloadJob;
        this.upsert(job);
        if (isTerminal(job)) stop();
      } catch {
        // A malformed frame is dropped rather than tearing down a live
        // transfer: the next frame carries the whole state anyway.
      }
    });

    source.onerror = () => {
      // EventSource retries on its own. Only give up once the browser has,
      // and even then the TRANSFER continues server-side — re-opening the
      // drawer re-attaches.
      if (source.readyState === EventSource.CLOSED) stop();
    };
  }

  /** Re-attaches to everything the server still knows about. */
  async resync(): Promise<void> {
    const res = await authedFetch('/api/desktop/downloads', {
      headers: { Accept: 'application/json' },
    });
    if (!res.ok) return;
    const body = (await res.json()) as { downloads?: DesktopDownloadJob[] };
    for (const job of body.downloads ?? []) {
      this.upsert(job);
      if (!isTerminal(job)) this.watch(job.id);
    }
  }

  /** Cancels a transfer. The server stops it and removes the partial file. */
  async cancel(id: string): Promise<void> {
    await authedFetch(`/api/desktop/downloads/${id}`, { method: 'DELETE' });
  }

  /** Removes a terminal card. A view operation: it cancels nothing. */
  dismiss(id: string): void {
    const job = this.jobs.find((j) => j.id === id);
    if (job && !isTerminal(job)) return;
    this.streams.get(id)?.();
    this.jobs = this.jobs.filter((j) => j.id !== id);
    this.emit();
  }

  /** Removes every terminal card, leaving running transfers alone. */
  dismissSettled(): number {
    const before = this.jobs.length;
    const next = this.jobs.filter((j) => !isTerminal(j));
    if (next.length === before) return 0;
    this.jobs = next;
    this.emit();
    return before - next.length;
  }
}

/** Formats an ETA. -1 (unknown) renders BLANK, never "0s". */
export function formatEta(seconds: number): string {
  if (seconds < 0) return '';
  if (seconds < 60) return `${Math.max(1, Math.round(seconds))}s`;
  const mins = Math.floor(seconds / 60);
  if (mins < 60) return `${mins}m ${Math.round(seconds % 60)}s`;
  return `${Math.floor(mins / 60)}h ${mins % 60}m`;
}

/**
 * start begins a Go-side download.
 *
 * Throws on refusal — a damaged file, an unwritable directory — so the caller
 * can report it. Every precondition is checked server-side BEFORE a byte
 * moves, so a rejection here means nothing was written.
 */
export async function startDesktopDownload(
  fileId: string,
  dir?: string,
): Promise<DesktopDownloadJob> {
  const res = await authedFetch('/api/desktop/downloads', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ file_id: fileId, ...(dir ? { dir } : {}) }),
  });
  if (!res.ok) {
    const body = (await res.json().catch(() => ({}))) as { message?: string };
    throw new Error(body.message ?? 'Could not start that download.');
  }
  return (await res.json()) as DesktopDownloadJob;
}
