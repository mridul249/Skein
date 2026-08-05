import { useEffect, useRef } from 'react';
import { Download, Lock, Trash2, X } from 'lucide-react';
import clsx from 'clsx';

import type { Drive, FileItem } from '../lib/api';
import {
  DRIVE_BG,
  UNKNOWN_DRIVE_BG,
  bytes,
  relativeTime,
  shardColor,
  shardOrdinal,
} from '../lib/format';
import { evidenceAge, isDamaged } from '../lib/health';
import { SHARD_STATE, integritySummary, shardState } from '../lib/shards';
import { AccountChip } from './AccountChip';
import { FilePreview } from './FilePreview';

/**
 * The file detail drawer: what a selected file actually is, an ordered set of
 * shards each with an address and a state.
 *
 * **This is deliberately not a modal, and must not be made into one.** The
 * modal in this codebase traps focus, dims the page and returns focus on close,
 * and every one of those is correct there and wrong here:
 *
 *  - **No focus trap.** The listing behind the drawer stays live, and picking a
 *    different file is the main thing someone does with it open. A trap would
 *    make that impossible by keyboard.
 *  - **No dim.** Dimming says "deal with me first". A drawer is a second pane,
 *    not an interruption.
 *  - **Selecting another row swaps the contents**, rather than closing and
 *    reopening — the drawer stays mounted for as long as anything is selected,
 *    so focus is moved in once on open and not stolen again on every swap.
 *
 * What it shares with the modal: `Esc` closes it, and focus returns to the row
 * that opened it.
 *
 * Its shell is `fixed`, which is also what makes it immune to known issue #27.
 * Nothing above it in the tree has an `overflow` that can clip it, because for
 * layout purposes there is nothing above it.
 *
 * Design.md §5, "addresses, not fragments". This is the screen where that rule
 * has to hold hardest, because it is the one that could most easily make a
 * striped file look damaged. Three things do the work:
 *
 *  - The **map** tiles the file exactly. Shards run in index order, edge to
 *    edge, proportional to real size, with a 1px hairline at each boundary and
 *    no gutter. A bar that tiles perfectly reads as a planned decomposition;
 *    gaps read as pieces that fell off something.
 *  - Every shard carries its **account number**, so the list reads as an
 *    inventory rather than as confetti.
 *  - **Integrity is stated, never inferred**, and by a glyph as well as a
 *    colour. See `shards.ts` for why nothing reads "verified" yet.
 */
export function FileDetail({
  file,
  drives,
  open,
  onClose,
  onDownload,
  onTrash,
  onPurgeDamaged,
}: {
  /** The selected file. Kept mounted while `open`, so contents can swap. */
  file: FileItem;
  drives: Drive[];
  open: boolean;
  onClose: () => void;
  /** Permanently removes a file whose shards are confirmed gone. */
  onPurgeDamaged: (file: FileItem) => void;
  onDownload: (file: FileItem) => void;
  onTrash: (file: FileItem) => void;
}) {
  const panelRef = useRef<HTMLElement>(null);
  const wasOpen = useRef(false);

  // Focus moves in on open and is then left alone. Re-focusing on every
  // contents swap would yank the caret away from someone arrowing down the
  // listing with the drawer open, which is the main way it gets used.
  useEffect(() => {
    if (open && !wasOpen.current) panelRef.current?.focus();
    wasOpen.current = open;
  }, [open]);

  // Read from the PERSISTED status, so it survives a reload. Before the schema
  // bundle this state existed only inside one reconcile response.
  const damaged = isDamaged(file);
  const checked = evidenceAge(file);

  const states = file.shards.map((s) => shardState(s.account_id, drives));
  const summary = integritySummary(states);
  const accounts = new Set(
    file.shards.map((s) => shardColor(s.account_id, drives)).filter(Boolean),
  ).size;

  const driveLabel = (accountId: string | null): string => {
    const drive = drives.find((d) => d.id === accountId);
    if (!drive) return 'Drive not connected';
    return drive.email || drive.display_name || 'drive';
  };

  return (
    <aside
      ref={panelRef}
      aria-label={`Details for ${file.name}`}
      tabIndex={-1}
      onKeyDown={(e) => {
        if (e.key === 'Escape') {
          e.stopPropagation();
          onClose();
        }
      }}
      className={clsx(
        // Fixed, so no ancestor overflow can clip it and so opening it never
        // reflows the listing that triggered it.
        'fixed inset-y-0 right-0 z-40 flex flex-col border-l border-border bg-surface',
        'overflow-y-auto outline-none',
        // Full width below lg. A 400px pane beside a listing needs the listing
        // to stay readable: at lg (1024px) the content column is ~700px and a
        // pane leaves ~300px, which is already the narrowest a file table can
        // be and still show a name plus its shards. Below that the pane takes
        // the screen and the listing waits.
        'w-full lg:w-[26rem]',
        'transition-transform duration-panel ease-out motion-reduce:transition-none',
        open ? 'translate-x-0' : 'translate-x-full',
      )}
    >
      <header className="flex items-start justify-between gap-3 border-b border-line p-4">
        <div className="min-w-0">
          <h2 className="break-words text-body font-semibold text-text">{file.name}</h2>
          <p className="mt-1 flex flex-wrap items-center gap-x-2 text-caption text-muted">
            <span className="tabular">{bytes(file.size_bytes)}</span>
            <span aria-hidden className="text-faint">·</span>
            <span className="tabular">
              {file.shards.length} {file.shards.length === 1 ? 'shard' : 'shards'}
            </span>
            {accounts > 0 && (
              <>
                <span aria-hidden className="text-faint">·</span>
                <span className="tabular">
                  {accounts} {accounts === 1 ? 'drive' : 'drives'}
                </span>
              </>
            )}
            <span aria-hidden className="text-faint">·</span>
            <span className="tabular">added {relativeTime(file.created_at)}</span>
          </p>
        </div>
        <button
          type="button"
          onClick={onClose}
          aria-label="Close details"
          className="shrink-0 rounded p-1.5 text-muted transition-colors duration-hover hover:bg-raised hover:text-text"
        >
          <X size={16} aria-hidden />
        </button>
      </header>

      <div className="flex-1 space-y-5 p-4">
        {/* Stated before anything else in the drawer: the file's condition
            changes what every control below it means. */}
        {damaged && (
          <section
            data-damaged-panel
            className="rounded border border-danger/40 bg-danger/10 p-3"
          >
            <p className="text-body text-danger">
              <span aria-hidden="true">✕ </span>
              {file.status === 'corrupted'
                ? 'Every shard of this file is missing.'
                : 'Some shards of this file are missing.'}
            </p>
            <p className="mt-1 text-caption text-muted">
              They were most likely deleted from Drive outside Skein. The file
              cannot be downloaded, and the missing shards cannot be rebuilt.
              {checked ? ` Last checked ${checked}.` : ''}
            </p>
          </section>
        )}

        <FilePreview file={file} />

        <section>
          <h3 className="mb-2 text-label font-semibold text-muted">Layout on your drives</h3>

          {/*
            The map. `gap-px` over a --line background draws the hairline
            between shards without opening a gutter: the segments still tile
            the full width, so the bar reads as one complete object.
          */}
          <div
            className="flex h-8 w-full gap-px overflow-hidden rounded bg-line"
            role="img"
            aria-label={`${file.shards.length} shards in order: ${file.shards
              .map((s, i) => {
                const n = shardOrdinal(s.account_id, drives);
                return `shard ${s.index} on ${n === null ? 'an unknown drive' : `drive ${n}`}, ${SHARD_STATE[states[i]!]!.word.toLowerCase()}`;
              })
              .join('; ')}`}
          >
            {file.shards.map((shard) => {
              const color = shardColor(shard.account_id, drives);
              const share =
                file.size_bytes > 0 ? (shard.plain_size_bytes / file.size_bytes) * 100 : 100;
              return (
                <div
                  key={shard.index}
                  // No minimum width here, unlike the pooled quota bar: these
                  // segments must sum to exactly the file, and a floor would
                  // make the map lie about proportions to stay visible.
                  style={{ width: `${share}%` }}
                  className={clsx(
                    'flex items-center justify-center',
                    color ? DRIVE_BG[color] : UNKNOWN_DRIVE_BG,
                  )}
                  title={`Shard ${shard.index} · ${bytes(shard.plain_size_bytes)}`}
                >
                  <span className="tabular text-[10px] font-semibold text-canvas">
                    {shardOrdinal(shard.account_id, drives) ?? '?'}
                  </span>
                </div>
              );
            })}
          </div>

          <p className={clsx('mt-2 text-caption', summary.tone)}>{summary.word}</p>
        </section>

        <section>
          <h3 className="mb-2 text-label font-semibold text-muted">Shards</h3>
          <ul className="divide-y divide-line">
            {file.shards.map((shard, i) => {
              const state = SHARD_STATE[states[i]!]!;
              return (
                <li key={shard.index} className="flex items-center gap-2.5 py-2">
                  <AccountChip ordinal={shardOrdinal(shard.account_id, drives)} size="sm" />
                  <span className="tabular shrink-0 text-caption text-faint">
                    #{shard.index}
                  </span>
                  <span className="min-w-0 flex-1 truncate text-caption text-muted">
                    {driveLabel(shard.account_id)}
                  </span>
                  <span className="tabular shrink-0 text-caption text-muted">
                    {bytes(shard.plain_size_bytes)}
                  </span>
                  {/* Never colour alone: glyph and word both carry the state. */}
                  <span
                    className={clsx('flex shrink-0 items-center gap-1 text-caption', state.tone)}
                    title={state.hint}
                  >
                    <span aria-hidden>{state.glyph}</span>
                    <span className="sr-only">{state.word}</span>
                  </span>
                </li>
              );
            })}
          </ul>
        </section>

        <section className="space-y-1.5">
          <h3 className="text-label font-semibold text-muted">Integrity</h3>
          <p className="flex items-center gap-2 text-caption text-muted">
            {file.is_encrypted ? (
              <>
                <Lock size={13} className="shrink-0 text-success" aria-hidden />
                Encrypted with AES-256-GCM before leaving this machine.
              </>
            ) : (
              <span className="text-warning">Stored without encryption.</span>
            )}
          </p>
          {file.sha256 && (
            <p className="tabular break-all text-caption text-faint">
              sha256 {file.sha256}
            </p>
          )}
        </section>
      </div>

      <footer className="flex gap-2 border-t border-line p-4">
        {/* A damaged file gets NO download button. Its shards are gone, so the
            server refuses to sign a URL for it and the button could only ever
            fail — offering it is the UI lying twice, once by implying the file
            is fetchable and once by blaming the failure on something else.
            Purge takes its place, since removing the row is the only action
            that can actually succeed. */}
        {damaged ? (
          <button
            type="button"
            className="btn-ghost flex-1 text-danger"
            onClick={() => onPurgeDamaged(file)}
          >
            <Trash2 size={15} aria-hidden />
            Remove permanently
          </button>
        ) : (
          <button type="button" className="btn-ghost flex-1" onClick={() => onDownload(file)}>
            <Download size={15} aria-hidden />
            Download
          </button>
        )}
        <button
          type="button"
          className="btn-quiet"
          aria-label={`Move ${file.name} to trash`}
          onClick={() => onTrash(file)}
        >
          <Trash2 size={15} aria-hidden />
        </button>
      </footer>
    </aside>
  );
}
