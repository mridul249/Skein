/**
 * Client side of the bulk endpoints.
 *
 * The server returns one result per file. This module's job is to keep that
 * honesty intact rather than flattening it into a toast: seven of ten
 * succeeding is neither a success nor a failure, and a UI that picks one is
 * lying about the other three.
 */
import { type BulkResponse, type BulkResult, api } from './api';
import { chunk } from './selection';

export interface BulkOutcome {
  results: BulkResult[];
  succeeded: number;
  failed: number;
  /** Ids that failed, so the caller can keep them selected for a retry. */
  failedIds: string[];
}

function merge(responses: BulkResponse[]): BulkOutcome {
  const results = responses.flatMap((r) => r.results);
  const failedIds = results.filter((r) => !r.ok).map((r) => r.file_id);
  return {
    results,
    succeeded: results.filter((r) => r.ok).length,
    failed: failedIds.length,
    failedIds,
  };
}

/**
 * runBulkDelete deletes ids, chunked to the server's per-request cap.
 *
 * Batches run SEQUENTIALLY. They all contend for the same Drive worker pool
 * server-side, so firing them in parallel would not go faster — it would just
 * make the rate limiter's job harder and the partial-failure picture more
 * confusing.
 *
 * A batch that fails at the transport level (network dropped, 500) is turned
 * into per-file failures rather than thrown, so one bad batch does not discard
 * the results of the batches that already succeeded.
 */
export async function runBulkDelete(
  ids: readonly string[],
  opts: { permanent?: boolean } = {},
): Promise<BulkOutcome> {
  const responses: BulkResponse[] = [];
  const send = opts.permanent ? api.bulkPurge : api.bulkDelete;

  for (const batch of chunk(ids)) {
    try {
      responses.push(await send(batch));
    } catch (err) {
      const message = err instanceof Error ? err.message : 'The request failed.';
      responses.push({
        results: batch.map((id) => ({
          file_id: id,
          ok: false,
          code: 'request_failed',
          error: message,
        })),
        succeeded: 0,
        failed: batch.length,
      });
    }
  }
  return merge(responses);
}

/** runEmptyTrash purges the trash in one call. */
export async function runEmptyTrash(): Promise<BulkOutcome> {
  return merge([await api.emptyTrash()]);
}

/**
 * summarise renders the outcome as one honest sentence.
 *
 * Never "Deleted 10 files" when three failed, and never a bare error when
 * seven worked. The count of each is the message.
 */
export function summarise(outcome: BulkOutcome, verb = 'Deleted'): string {
  const { succeeded, failed } = outcome;
  if (failed === 0) {
    return `${verb} ${succeeded} ${succeeded === 1 ? 'file' : 'files'}.`;
  }
  if (succeeded === 0) {
    return `Could not process ${failed} ${failed === 1 ? 'file' : 'files'}.`;
  }
  return `${verb} ${succeeded} of ${succeeded + failed} files. ${failed} failed.`;
}

/** Groups failures by reason so the UI can list them compactly. */
export function failureReasons(outcome: BulkOutcome): { reason: string; count: number }[] {
  const byReason = new Map<string, number>();
  for (const r of outcome.results) {
    if (r.ok) continue;
    const reason = r.error ?? 'Unknown error.';
    byReason.set(reason, (byReason.get(reason) ?? 0) + 1);
  }
  return [...byReason.entries()]
    .map(([reason, count]) => ({ reason, count }))
    .sort((a, b) => b.count - a.count);
}

/**
 * DOWNLOAD_STAGGER_MS spaces sequential downloads apart.
 *
 * Chrome treats a rapid burst of programmatic downloads as a popup/abuse
 * pattern and silently blocks all but the first. A short gap keeps each click
 * attributable to the original user gesture chain. It is not a rate limit —
 * the server has its own — it is purely about the browser's heuristic.
 */
export const DOWNLOAD_STAGGER_MS = 350;

const wait = (ms: number) => new Promise<void>((r) => setTimeout(r, ms));

/**
 * runBulkDownload downloads files one at a time through the ordinary
 * single-file path.
 *
 * CLIENT-DRIVEN BY DESIGN, not for want of a zip endpoint. Server-side zip
 * streaming would hold one connection open for the whole transfer while
 * interleaving every shard read through the Drive rate limiter, and a 429
 * partway through leaves the user with a truncated archive under a filename
 * promising a complete one. Sequential per-file downloads are resumable, are
 * individually cancellable, and keep per-shard digest verification on the
 * normal read path.
 *
 * `download` is injected so this is testable without a DOM.
 */
export async function runBulkDownload(
  files: readonly { id: string; name: string }[],
  download: (file: { id: string; name: string }) => Promise<void>,
  opts: { staggerMs?: number; onProgress?: (done: number, total: number) => void } = {},
): Promise<{ started: number; failed: { id: string; error: string }[] }> {
  const staggerMs = opts.staggerMs ?? DOWNLOAD_STAGGER_MS;
  const failed: { id: string; error: string }[] = [];
  let started = 0;

  for (const [i, file] of files.entries()) {
    try {
      await download(file);
      started++;
    } catch (err) {
      failed.push({
        id: file.id,
        error: err instanceof Error ? err.message : 'Could not start that download.',
      });
    }
    opts.onProgress?.(i + 1, files.length);
    // No trailing wait after the final file.
    if (i < files.length - 1 && staggerMs > 0) await wait(staggerMs);
  }

  return { started, failed };
}
