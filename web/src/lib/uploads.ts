/**
 * Upload jobs that outlive the route.
 *
 * Known issue #13: job state, including the AbortController, used to live in
 * Files.tsx local component state. Navigating to Drives unmounted the
 * component and lost the handle while the XHR kept streaming — the cause of
 * the 2026-07-30 reservation-leak incident, where a 15 GB upload carried on
 * holding a legitimate reservation with no UI attached to it.
 *
 * The fix is to lift the state above the router, not to abort on unmount:
 * aborting would throw away a healthy multi-hour upload because someone
 * clicked a tab, which is worse than the bug it fixes. The only abort is the
 * user's explicit cancel.
 *
 * This is a plain module rather than a React store on purpose. Nothing here
 * needs React, it can be tested without a DOM, and being outside the component
 * tree is precisely the property being bought.
 */

/** What the store needs from a File. The DOM File type satisfies it. */
export interface UploadFile {
  name: string;
  size: number;
}

/**
 * A job's lifecycle.
 *
 * `sending` and `finishing` are deliberately separate. See UploadJob.sent for
 * why the distinction is not cosmetic.
 */
export type UploadStatus = 'sending' | 'finishing' | 'done' | 'error' | 'cancelled';

export interface UploadJob {
  id: string;
  name: string;
  size: number;
  /**
   * Bytes handed to the browser's network stack — NOT bytes the server has
   * accepted and written to a drive.
   *
   * The difference is real and this type does not hide it. XHR reports upload
   * progress as the request body is flushed to the socket, so `sent` runs
   * ahead of the server: when it reaches `size` the browser is done talking,
   * but the server may still be pushing the final shards to Drive. That tail
   * is the `finishing` status rather than a bar sitting at 100% pretending to
   * be complete. Reporting true server-side progress would need the server to
   * publish it, which is a Session 7 concern (see Memory.md).
   */
  sent: number;
  status: UploadStatus;
  error?: string;
}

/** Performs the transfer. Injected so the store is testable without a network. */
export type Uploader = (
  file: UploadFile,
  folderId: string | null,
  onProgress: (sent: number, total: number) => void,
  signal: AbortSignal,
) => Promise<unknown>;

/** Called once a job reaches a terminal state, for cache invalidation. */
export type SettledListener = (job: UploadJob) => void;

// `finishing` is deliberately absent: every byte has been handed to the socket
// but the server is still writing shards to Drive, so it is still in flight —
// and it is exactly when a reload is most expensive and least obviously
// dangerous. A predicate keying on `sending` alone drops that window silently.
const TERMINAL: readonly UploadStatus[] = ['done', 'error', 'cancelled'];

/** isActive reports whether a job still has a request in flight. */
export function isActive(job: UploadJob): boolean {
  return !TERMINAL.includes(job.status);
}

export class UploadStore {
  private jobs: UploadJob[] = [];
  private readonly controllers = new Map<string, AbortController>();
  private readonly listeners = new Set<() => void>();
  private seq = 0;

  constructor(
    private readonly uploader: Uploader,
    private readonly onSettled: SettledListener = () => undefined,
  ) {}

  /** subscribe registers a change listener and returns its unsubscribe. */
  subscribe = (fn: () => void): (() => void) => {
    this.listeners.add(fn);
    return () => {
      this.listeners.delete(fn);
    };
  };

  /**
   * getSnapshot returns the current jobs.
   *
   * The array identity changes only when something actually changed, because
   * useSyncExternalStore treats a fresh identity as a new value and would
   * re-render forever otherwise.
   */
  getSnapshot = (): UploadJob[] => this.jobs;

  /** activeCount is how many jobs still have a request in flight. */
  activeCount(): number {
    return this.jobs.filter(isActive).length;
  }

  /** start queues a file and returns the new job's id. */
  start(file: UploadFile, folderId: string | null): string {
    const id = `upload-${++this.seq}`;
    const controller = new AbortController();
    this.controllers.set(id, controller);

    this.jobs = [
      ...this.jobs,
      { id, name: file.name, size: file.size, sent: 0, status: 'sending' },
    ];
    this.emit();

    void this.uploader(
      file,
      folderId,
      (sent, total) => {
        // A late progress event after cancellation must not resurrect the
        // job or drag it backwards out of a terminal state.
        this.patch(id, (job) =>
          isActive(job)
            ? { ...job, sent, status: sent >= total && total > 0 ? 'finishing' : 'sending' }
            : job,
        );
      },
      controller.signal,
    )
      .then(() => {
        this.controllers.delete(id);
        this.patch(id, (job) =>
          isActive(job) ? { ...job, sent: job.size, status: 'done' } : job,
        );
        this.settle(id);
      })
      .catch((err: unknown) => {
        this.controllers.delete(id);
        // An abort the user asked for is not a failure, and must not be
        // rendered as one.
        const aborted = controller.signal.aborted;
        const message = err instanceof Error ? err.message : 'Upload failed.';
        this.patch(id, (job) =>
          isActive(job)
            ? {
                ...job,
                status: aborted ? 'cancelled' : 'error',
                error: aborted ? undefined : message,
              }
            : job,
        );
        this.settle(id);
      });

    return id;
  }

  /**
   * cancel aborts a job at the user's request.
   *
   * This is the only abort in the system. Nothing cancels on unmount.
   */
  cancel(id: string): void {
    this.controllers.get(id)?.abort();
    this.controllers.delete(id);
    // Marked here as well as in the rejection handler: an uploader that never
    // rejects on abort would otherwise leave the job stuck in `sending`.
    this.patch(id, (job) => (isActive(job) ? { ...job, status: 'cancelled' } : job));
  }

  /** dismiss removes a settled job from the list. */
  dismiss(id: string): void {
    const job = this.jobs.find((j) => j.id === id);
    if (job && isActive(job)) return; // dismissing a live upload would orphan it again
    this.jobs = this.jobs.filter((j) => j.id !== id);
    this.emit();
  }

  private settle(id: string): void {
    const job = this.jobs.find((j) => j.id === id);
    if (job) this.onSettled(job);
  }

  private patch(id: string, fn: (job: UploadJob) => UploadJob): void {
    let changed = false;
    const next = this.jobs.map((job) => {
      if (job.id !== id) return job;
      const updated = fn(job);
      if (updated !== job) changed = true;
      return updated;
    });
    if (!changed) return;
    this.jobs = next;
    this.emit();
  }

  private emit(): void {
    for (const fn of this.listeners) fn();
  }
}

/** The parts of window the unload guard uses. */
export interface UnloadTarget {
  addEventListener(type: 'beforeunload', fn: (e: BeforeUnloadEvent) => void): void;
  removeEventListener(type: 'beforeunload', fn: (e: BeforeUnloadEvent) => void): void;
}

/**
 * installUnloadGuard warns before a page unload that would kill an upload.
 *
 * This warning is correct for uploads and wrong for downloads. A real unload
 * kills the XHR and the server releases the reservation, so the bytes are
 * genuinely lost. A download is browser-managed since the capability-URL
 * change and survives the tab closing outright, so warning about one would be
 * a lie. Nothing in this module knows about downloads, which is how it stays
 * that way.
 *
 * The listener is attached only while something is in flight, so a quiet app
 * never prompts.
 */
export function installUnloadGuard(store: UploadStore, target: UnloadTarget): () => void {
  let attached = false;

  const onBeforeUnload = (e: BeforeUnloadEvent) => {
    e.preventDefault();
    // Legacy browsers need returnValue set to trigger the prompt.
    e.returnValue = '';
  };

  const sync = () => {
    const wanted = store.activeCount() > 0;
    if (wanted === attached) return;
    if (wanted) target.addEventListener('beforeunload', onBeforeUnload);
    else target.removeEventListener('beforeunload', onBeforeUnload);
    attached = wanted;
  };

  const unsubscribe = store.subscribe(sync);
  sync();

  return () => {
    unsubscribe();
    if (attached) target.removeEventListener('beforeunload', onBeforeUnload);
    attached = false;
  };
}
