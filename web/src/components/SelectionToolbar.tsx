import { type BulkOutcome, failureReasons, summarise } from '../lib/bulk';

/**
 * The bar that appears once something is selected.
 *
 * Appears rather than sits permanently disabled: a toolbar full of greyed-out
 * buttons is noise on every page load, and the selection is the thing that
 * makes those actions meaningful.
 */
export function SelectionToolbar({
  count,
  busy,
  onDelete,
  onDownload,
  onClear,
  deleteLabel = 'Delete',
  showDownload = true,
  extra,
}: {
  count: number;
  busy?: boolean;
  onDelete: () => void;
  onDownload?: () => void;
  onClear: () => void;
  deleteLabel?: string;
  showDownload?: boolean;
  extra?: React.ReactNode;
}) {
  if (count === 0) return null;

  return (
    <div
      role="toolbar"
      aria-label="Selection actions"
      className="mb-4 flex flex-wrap items-center gap-3 rounded border border-border bg-raised px-4 py-2"
    >
      <span className="tabular text-data-sm text-text">
        {count} selected
      </span>

      <div className="flex flex-wrap items-center gap-2">
        {showDownload && onDownload && (
          <button type="button" className="btn-ghost" onClick={onDownload} disabled={busy}>
            Download
          </button>
        )}
        <button type="button" className="btn-danger" onClick={onDelete} disabled={busy}>
          {busy ? 'Working…' : deleteLabel}
        </button>
        {extra}
      </div>

      <button
        type="button"
        onClick={onClear}
        disabled={busy}
        className="ml-auto rounded px-2 py-1 text-caption text-muted
                   transition-colors duration-hover hover:bg-canvas hover:text-text"
      >
        Clear selection
      </button>
    </div>
  );
}

/**
 * The result of a bulk operation, rendered honestly.
 *
 * Seven of ten succeeding is not a success toast and not an error toast. This
 * shows the counts, then the distinct reasons with how many files each hit —
 * grouped, so ten rate-limited files are one line rather than ten.
 *
 * The failed files stay selected (the caller does that), so Retry is one
 * click and acts on exactly the ones that did not go.
 */
export function BulkOutcomeNotice({
  outcome,
  verb,
  onRetry,
  onDismiss,
}: {
  outcome: BulkOutcome;
  verb?: string;
  onRetry?: () => void;
  onDismiss: () => void;
}) {
  const reasons = failureReasons(outcome);
  const partial = outcome.failed > 0 && outcome.succeeded > 0;

  return (
    <div
      role="status"
      className={
        outcome.failed === 0
          ? 'mb-4 rounded border border-border bg-raised p-3'
          : 'mb-4 rounded border border-warning bg-raised p-3'
      }
    >
      <div className="flex items-baseline justify-between gap-3">
        <p className="text-body text-text">{summarise(outcome, verb)}</p>
        <button
          type="button"
          onClick={onDismiss}
          className="shrink-0 rounded px-2 py-1 text-caption text-muted
                     transition-colors duration-hover hover:bg-canvas hover:text-text"
        >
          Dismiss
        </button>
      </div>

      {reasons.length > 0 && (
        <ul className="mt-2 space-y-1">
          {reasons.map((r) => (
            <li key={r.reason} className="text-caption text-muted">
              <span className="tabular text-text">{r.count}</span>: {r.reason}
            </li>
          ))}
        </ul>
      )}

      {outcome.failed > 0 && onRetry && (
        <button type="button" className="btn-ghost mt-3" onClick={onRetry}>
          Retry {outcome.failed} failed
        </button>
      )}

      {partial && (
        <p className="mt-2 text-caption text-faint">
          The files that failed are still selected.
        </p>
      )}
    </div>
  );
}

/**
 * A tri-state header checkbox.
 *
 * `indeterminate` is a DOM property, not an attribute, so it has to be set
 * through a ref — React will not apply it from JSX.
 */
export function HeaderCheckbox({
  state,
  onChange,
  label,
}: {
  state: 'none' | 'some' | 'all';
  onChange: (checked: boolean) => void;
  label: string;
}) {
  return (
    <input
      type="checkbox"
      className="checkbox"
      aria-label={label}
      checked={state === 'all'}
      ref={(el) => {
        if (el) el.indeterminate = state === 'some';
      }}
      onChange={(e) => onChange(e.target.checked)}
    />
  );
}
