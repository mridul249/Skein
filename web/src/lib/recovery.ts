/**
 * Recovery logic: how to describe manifest coverage and a restore, in words
 * that do not overstate what happened.
 *
 * Pure functions over report shapes, so the wording — which is the part most
 * likely to lie — is covered by `node --test` rather than only by looking at
 * it.
 */
import type { BackfillReport, ReconstructReport } from './api';

/** What the drives are, as a recovery source. */
export type CoverageVerdict = 'none' | 'partial' | 'full' | 'unknown';

/**
 * coverageVerdict answers the only question that matters: if the database were
 * lost right now, would the drives bring it back?
 *
 * `unknown` when any drive could not be listed. NOT folded into `partial`: a
 * user told "some of your files are covered" will act very differently from
 * one told "I could not check two of your drives", and the second is the truth
 * when a scan was incomplete.
 */
export function coverageVerdict(report: BackfillReport | null): CoverageVerdict {
  if (!report) return 'unknown';
  if (!report.complete || report.coverage.indeterminate > 0) return 'unknown';

  const { files, covered, damaged } = report.coverage;
  // Damaged files are excluded from backfill by design, so they do not count
  // against coverage — a file whose shards are gone is not made recoverable by
  // a manifest.
  const recoverable = files - damaged;
  if (recoverable === 0) return covered > 0 ? 'full' : 'none';
  if (covered === 0) return 'none';
  return covered >= recoverable ? 'full' : 'partial';
}

/**
 * coverageSummary is the sentence shown under the heading.
 *
 * It never says "you are protected" on an incomplete scan, and it never
 * reports a bare fraction that hides why the remainder is missing.
 */
export function coverageSummary(report: BackfillReport | null): string {
  if (!report) return 'Checking your drives…';

  const c = report.coverage;
  const recoverable = c.files - c.damaged;

  if (!report.complete || c.indeterminate > 0) {
    return (
      `${c.covered} of ${recoverable} files have manifests, but some drives ` +
      `could not be checked, so the real figure may be higher or lower.`
    );
  }
  if (recoverable === 0) {
    return 'There are no files to protect yet.';
  }
  if (c.covered >= recoverable) {
    return `All ${recoverable} files have manifests on every drive holding their shards.`;
  }
  return `${c.covered} of ${recoverable} files have manifests. The rest cannot be recovered from the drives alone.`;
}

/** Counts of each per-file outcome, for the results list. */
export function tallyBackfill(report: BackfillReport): Record<string, number> {
  const out: Record<string, number> = {};
  for (const r of report.results) {
    out[r.state] = (out[r.state] ?? 0) + 1;
  }
  return out;
}

/** Drives that could not be listed, so the UI can name them rather than imply success. */
export function unscannedAccounts(report: BackfillReport | ReconstructReport): string[] {
  return report.accounts.filter((a) => !a.scanned).map((a) => a.reason || 'could not be scanned');
}

/**
 * restoreSummary describes what a reconstruct run did.
 *
 * INCOMPLETENESS IS STATED, NEVER FOLDED IN. A run that could not read every
 * drive gets its own sentence: a user told "recovered 12 files" after a
 * partial scan will stop looking for the rest, which is its own kind of loss.
 */
export function restoreSummary(report: ReconstructReport): string[] {
  const lines: string[] = [];

  if (report.files_recovered === 0 && report.files_already_present === 0) {
    lines.push('Nothing was recovered. No manifests were found on your drives.');
  } else {
    lines.push(
      `Recovered ${report.files_recovered} ${plural(report.files_recovered, 'file')}, ` +
        `${report.shards_recovered} ${plural(report.shards_recovered, 'shard')}` +
        (report.folders_recovered > 0
          ? ` and ${report.folders_recovered} ${plural(report.folders_recovered, 'folder')}.`
          : '.'),
    );
  }

  if (report.files_already_present > 0) {
    lines.push(
      `${report.files_already_present} ${plural(report.files_already_present, 'file was', 'files were')} ` +
        `already in the database and left untouched.`,
    );
  }
  if (report.manifests_unreadable > 0) {
    lines.push(
      `${report.manifests_unreadable} ${plural(report.manifests_unreadable, 'manifest')} could not be read. ` +
        `Those files exist but were not recovered.`,
    );
  }
  // SAID PLAINLY, BEFORE the generic incompleteness line.
  //
  // A file whose shards could not be placed is listable, previewable and
  // undownloadable — strictly worse than not recovering it, because it looks
  // fine. A live user was left in exactly that state on 2026-08-06 by a
  // summary that read "Recovered 7 files, 0 shards" and said nothing about
  // what that meant. The count is not enough; the consequence has to be
  // spelled out.
  if (report.shards_unresolved > 0) {
    lines.push(
      `${report.shards_unresolved} ${plural(report.shards_unresolved, 'shard')} could not be located ` +
        `on any drive that was read, so some files here cannot be downloaded yet. ` +
        `Connect every drive you used and run this again to fill in what is missing.`,
    );
  }
  if (!report.complete) {
    lines.push(
      'This run was incomplete. Some drives could not be scanned, so there may be more to recover. ' +
        'Running it again is safe.',
    );
  }
  return lines;
}

/**
 * isWarningLine reports whether a restoreSummary line is bad news.
 *
 * THE PREDICATE LIVES WITH THE SENTENCES IT MATCHES. RecoveryPanel used to
 * inline `/incomplete/.test(line)`, which silently mis-styled the
 * unplaceable-shard sentence added later: "some files here cannot be
 * downloaded yet" rendered in muted grey, the calmest styling on the panel,
 * for the most alarming thing it can say. A component matching on wording it
 * does not own will drift from it every time the wording grows.
 */
export function isWarningLine(line: string): boolean {
  return /incomplete|could not be located|could not be read/.test(line);
}

function plural(n: number, one: string, many?: string): string {
  if (n === 1) return one;
  return many ?? `${one}s`;
}
