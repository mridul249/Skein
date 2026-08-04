import { useCallback, useEffect, useRef, useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import {
  ChevronRight,
  Download,
  Folder as FolderIcon,
  FolderPlus,
  Trash2,
  Upload,
} from 'lucide-react';
import clsx from 'clsx';

import { ApiError, api, type FileItem, type Folder } from '../lib/api';
import { QuotaBar } from '../components/QuotaBar';
import { ShardMap } from '../components/ShardMap';
import { Modal } from '../components/Modal';
import { FileDetail } from '../components/FileDetail';
import { bytes, relativeTime } from '../lib/format';
import { useUploads } from '../lib/uploads-context';
import {
  BulkOutcomeNotice,
  HeaderCheckbox,
  SelectionToolbar,
} from '../components/SelectionToolbar';
import { runBulkDelete, runBulkDownload, type BulkOutcome } from '../lib/bulk';
import * as sel from '../lib/selection';
import { useDownloads } from '../lib/downloads-context';

export function Files() {
  const qc = useQueryClient();
  const [folderId, setFolderId] = useState<string | null>(null);
  const [dragging, setDragging] = useState(false);
  const [banner, setBanner] = useState('');
  /** Open dialogs. Only one can be open at a time by construction. */
  const [naming, setNaming] = useState(false);
  const [deleting, setDeleting] = useState<Folder | null>(null);
  /** The file whose detail drawer is open, by id. */
  const [selectedId, setSelectedId] = useState<string | null>(null);

  // Bulk selection, distinct from `selectedId` (which drives the detail
  // drawer). A set of ids rather than indices, so sorting and filtering cannot
  // reassign it to different files.
  const [selection, setSelection] = useState<sel.SelectionState>(sel.EMPTY);
  const [bulkBusy, setBulkBusy] = useState(false);
  const [bulkOutcome, setBulkOutcome] = useState<BulkOutcome | null>(null);
  // Focus returns to the row that opened the drawer. The row is found by id
  // rather than held as a node, because the listing re-renders underneath and
  // a captured element can be detached by the time it is needed — focusing a
  // detached node throws focus away entirely.
  const closeDrawer = useCallback(() => {
    const id = selectedId;
    setSelectedId(null);
    if (!id) return;
    requestAnimationFrame(() => {
      document.querySelector<HTMLElement>(`[data-row="${id}"] button`)?.focus();
    });
  }, [selectedId]);
  const inputRef = useRef<HTMLInputElement>(null);
  const { start: startUploadJob } = useUploads();
  const { start: startDownloadJob, fail: failDownloadJob } = useDownloads();

  const { data: quota } = useQuery({ queryKey: ['quota'], queryFn: api.quota });
  const { data: drivesData } = useQuery({ queryKey: ['drives'], queryFn: api.listDrives });
  const { data: foldersData } = useQuery({ queryKey: ['folders'], queryFn: api.listFolders });
  const {
    data: filesData,
    isLoading,
    error: listError,
  } = useQuery({
    queryKey: ['files', folderId],
    queryFn: () => api.listFiles(folderId),
  });

  const drives = drivesData?.accounts ?? [];
  const folders = foldersData?.folders ?? [];
  const files = filesData?.files ?? [];
  const visibleIds = files.map((f) => f.id);

  // Ids that have left the listing are dropped, so a file deleted elsewhere
  // cannot linger in the selection and reappear in the next bulk request.
  useEffect(() => {
    setSelection((s) => sel.keepOnly(s, visibleIds));
    // visibleIds is derived; joining it gives a stable dependency.
  }, [visibleIds.join(',')]);

  // Navigating between folders clears the selection: acting on rows the user
  // can no longer see is never what they meant.
  useEffect(() => {
    setSelection(sel.clear());
  }, [folderId]);
  const children = folders.filter((f) => f.parent_id === folderId);

  const refresh = useCallback(() => {
    void qc.invalidateQueries({ queryKey: ['files'] });
    void qc.invalidateQueries({ queryKey: ['quota'] });
    void qc.invalidateQueries({ queryKey: ['folders'] });
  }, [qc]);

  // The job list lives above the router, so navigating away no longer
  // orphans an in-flight upload (#13). Nothing here aborts on unmount.
  const startUpload = useCallback(
    (file: File) => startUploadJob(file, folderId),
    [folderId, startUploadJob],
  );

  // The key handler is registered once, so it must not close over a stale
  // listing. A ref keeps it reading the current one without re-binding.
  const filesRef = useRef<FileItem[]>([]);
  filesRef.current = files;

  // Design.md §8: U uploads, Del trashes, / searches. Shortcuts are ignored
  // while a text field has focus, or typing a filename triggers them.
  useEffect(() => {
    function onKey(e: KeyboardEvent) {
      const target = e.target as HTMLElement | null;
      if (target && ['INPUT', 'TEXTAREA', 'SELECT'].includes(target.tagName)) return;
      if (e.metaKey || e.ctrlKey || e.altKey) return;

      if (e.key === 'u' || e.key === 'U') {
        e.preventDefault();
        inputRef.current?.click();
        return;
      }
      if (e.key === 'Escape') {
        closeDrawer();
        return;
      }
      // Arrow keys walk the listing whether or not anything is selected yet,
      // so the detail pane is reachable without touching the mouse.
      if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
        if (filesRef.current.length === 0) return;
        e.preventDefault();
        setSelectedId((current) => {
          const list = filesRef.current;
          const at = list.findIndex((f) => f.id === current);
          const next =
            e.key === 'ArrowDown'
              ? Math.min(list.length - 1, at + 1)
              : Math.max(0, at <= 0 ? 0 : at - 1);
          return list[next]?.id ?? current;
        });
      }
    }
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [closeDrawer]);

  const trash = useMutation({
    mutationFn: (id: string) => api.trashFile(id),
    onSuccess: refresh,
    onError: (err: unknown) =>
      setBanner(err instanceof ApiError ? err.message : 'Could not move that to trash.'),
  });

  const newFolder = useMutation({
    mutationFn: (name: string) => api.createFolder(name, folderId),
    onSuccess: refresh,
    onError: (err: unknown) =>
      setBanner(err instanceof ApiError ? err.message : 'Could not create that folder.'),
  });

  // Was a bare `api.deleteFolder(...).then(refresh)` behind a window.confirm,
  // so a failure went nowhere — no banner, no retry, the row just stayed.
  const removeFolder = useMutation({
    mutationFn: (id: string) => api.deleteFolder(id),
    onSuccess: refresh,
    onError: (err: unknown) =>
      setBanner(err instanceof ApiError ? err.message : 'Could not delete that folder.'),
  });

  /**
   * Hands the transfer to the browser rather than running it in the tab.
   *
   * Only the short capability URL crosses JS; the bytes never do. The browser
   * streams straight to disk and keeps going if this tab closes — which is
   * also why no beforeunload warning belongs on this path.
   *
   * No real progress or completion signal is possible here: once `a.click()`
   * fires, the transfer is entirely the browser/webview's, and JS is never
   * told anything more about it — that silence is what keeps this path off
   * the heap (known issue #15). `startDownloadJob`/DownloadList only record
   * that a download was asked for, so the desktop build (which, unlike a
   * real browser, has no download manager UI of its own) has somewhere to
   * show that; the user dismisses it themselves.
   */
  async function download(file: FileItem) {
    const jobId = startDownloadJob(file.name);
    try {
      const url = await api.contentURL(file.id);
      const a = document.createElement('a');
      a.href = url;
      // The server already sends Content-Disposition: attachment with the
      // real name. This is the same-origin hint for the browser, not the
      // authority on the filename.
      a.download = file.name;
      a.click();
    } catch (err) {
      const message = err instanceof ApiError ? err.message : 'Could not download that file.';
      failDownloadJob(jobId, message);
      setBanner(message);
    }
  }

  const selectedIds = [...selection.ids];

  /** Runs a bulk delete and keeps the failures selected for a one-click retry. */
  async function bulkDelete(ids: string[]) {
    if (ids.length === 0) return;
    setBulkBusy(true);
    setBulkOutcome(null);
    try {
      // Trashes, not destroys — recoverable from the Trash view.
      const outcome = await runBulkDelete(ids);
      setBulkOutcome(outcome);
      // The successes are gone; the failures stay selected so Retry acts on
      // exactly the ones that did not go.
      setSelection(sel.replace(outcome.failedIds));
      await qc.invalidateQueries({ queryKey: ['files'] });
      await qc.invalidateQueries({ queryKey: ['quota'] });
    } finally {
      setBulkBusy(false);
    }
  }

  /**
   * Downloads the selection one file at a time.
   *
   * Sequential and staggered: Chrome treats a rapid burst of programmatic
   * downloads as an abuse pattern and silently blocks all but the first.
   */
  async function bulkDownload(ids: string[]) {
    const chosen = files.filter((f) => ids.includes(f.id));
    if (chosen.length === 0) return;

    setBulkBusy(true);
    setBanner(
      chosen.length === 1
        ? ''
        : `Starting ${chosen.length} downloads, one at a time…`,
    );
    try {
      const { failed } = await runBulkDownload(chosen, (f) =>
        download(files.find((x) => x.id === f.id) as FileItem),
      );
      setBanner(
        failed.length === 0
          ? ''
          : `${failed.length} of ${chosen.length} downloads could not be started.`,
      );
    } finally {
      setBulkBusy(false);
    }
  }

  // Resolved against the live listing rather than held as an object, so a
  // file that is trashed or navigated away from closes the drawer instead of
  // leaving a stale copy on screen.
  const selected = files.find((f) => f.id === selectedId) ?? null;


  const breadcrumb = buildBreadcrumb(folders, folderId);

  return (
    <div
      onDragOver={(e) => {
        e.preventDefault();
        setDragging(true);
      }}
      onDragLeave={(e) => {
        if (e.currentTarget === e.target) setDragging(false);
      }}
      onDrop={(e) => {
        e.preventDefault();
        setDragging(false);
        for (const file of Array.from(e.dataTransfer.files)) startUpload(file);
      }}
      className={clsx(
        'min-h-[60vh] transition-colors duration-hover',
        dragging && 'outline-dashed outline-2 outline-offset-4 outline-accent',
      )}
    >
      {/* The window.prompt replacement. A native prompt could not be styled,
          could not show which folder it was creating inside, and dropped the
          user into browser chrome mid-task. */}
      <Modal
        open={naming}
        title="New folder"
        confirmLabel="Create"
        prompt={{ label: 'Name', placeholder: 'projects' }}
        onCancel={() => setNaming(false)}
        onConfirm={(name) => {
          newFolder.mutate(name);
          setNaming(false);
        }}
      />

      {/* Design.md §7: name what goes, not "Are you sure?". */}
      <Modal
        open={deleting !== null}
        title={deleting ? `Delete ${deleting.name} and everything in it?` : ''}
        intent="danger"
        confirmLabel="Delete"
        onCancel={() => setDeleting(null)}
        onConfirm={() => {
          if (deleting) removeFolder.mutate(deleting.id);
          setDeleting(null);
        }}
      >
        Files inside this folder go to Trash, where you can still restore them.
      </Modal>

      <header className="mb-6 flex flex-wrap items-end justify-between gap-4">
        <div>
          <h1 className="text-title font-semibold text-text">Files</h1>
          <p className="mt-1 text-body text-muted">
            Encrypted here, then striped across your drives.
          </p>
        </div>
        <div className="flex gap-2">
          <button
            type="button"
            className="btn-ghost"
            onClick={() => setNaming(true)}
          >
            <FolderPlus size={15} aria-hidden />
            New folder
          </button>
          <button type="button" className="btn-primary" onClick={() => inputRef.current?.click()}>
            <Upload size={15} aria-hidden />
            Upload
          </button>
          <input
            ref={inputRef}
            type="file"
            multiple
            className="sr-only"
            onChange={(e) => {
              for (const file of Array.from(e.target.files ?? [])) startUpload(file);
              e.target.value = '';
            }}
          />
        </div>
      </header>

      {quota && (
        <div className="mb-5">
          <QuotaBar quota={quota} />
        </div>
      )}

      {banner && (
        <p role="alert" className="mb-4 rounded-md border border-danger/40 bg-danger/10 px-4 py-3 text-body text-danger">
          {banner}
        </p>
      )}

      {breadcrumb.length > 0 && (
        <nav aria-label="Breadcrumb" className="mb-3 flex items-center gap-1 text-label">
          <button type="button" className="text-accent" onClick={() => setFolderId(null)}>
            All files
          </button>
          {breadcrumb.map((f) => (
            <span key={f.id} className="flex items-center gap-1">
              <ChevronRight size={13} className="text-faint" aria-hidden />
              <button type="button" className="text-accent" onClick={() => setFolderId(f.id)}>
                {f.name}
              </button>
            </span>
          ))}
        </nav>
      )}

      {/*
        `relative` is load-bearing. Without it the card is not the containing
        block for its absolutely-positioned descendants, so the `sr-only`
        spans in the table header escape `overflow-x-auto` entirely and extend
        the *document's* scrollable width — the whole page scrolled sideways
        by 305px at 375px while the table looked correctly clipped.
      */}
      {bulkOutcome && (
        <BulkOutcomeNotice
          outcome={bulkOutcome}
          verb="Moved to trash"
          onDismiss={() => setBulkOutcome(null)}
          onRetry={
            bulkOutcome.failedIds.length > 0
              ? () => void bulkDelete(bulkOutcome.failedIds)
              : undefined
          }
        />
      )}

      <SelectionToolbar
        count={selectedIds.length}
        busy={bulkBusy}
        deleteLabel="Move to trash"
        onDelete={() => void bulkDelete(selectedIds)}
        onDownload={() => void bulkDownload(selectedIds)}
        onClear={() => setSelection(sel.clear())}
      />

      <div className="card relative md:overflow-x-auto">
        <table className="hidden w-full min-w-[34rem] border-collapse md:table">
          <thead>
            <tr className="border-b border-border text-left">
              <th scope="col" className="th w-10 pl-4">
                <HeaderCheckbox
                  state={sel.headerState(selection, visibleIds)}
                  label="Select all files"
                  onChange={(checked) =>
                    setSelection(checked ? sel.selectAll(visibleIds) : sel.clear())
                  }
                />
              </th>
              <th scope="col" className="th ">
                Name
              </th>
              <th scope="col" className="th w-28 text-right">
                Size
              </th>
              <th scope="col" className="th w-32">
                Stored
              </th>
              <th scope="col" className="th w-28">
                Added
              </th>
              <th scope="col" className="th w-24">
                <span className="sr-only">Actions</span>
              </th>
            </tr>
          </thead>
          <tbody>
            {children.map((folder) => (
              <tr
                key={folder.id}
                className="h-row border-b border-line transition-colors duration-hover last:border-0 hover:bg-raised"
              >
                {/* Folders are not bulk-selectable: deleting one is a
                    recursive operation with its own confirmation, not the
                    same action as deleting a set of files. The empty cell
                    keeps the columns aligned with the file rows. */}
                <td className="w-10 pl-4" />
                <td className="px-4">
                  <button
                    type="button"
                    className="flex items-center gap-2 text-body text-text"
                    onClick={() => setFolderId(folder.id)}
                  >
                    <FolderIcon size={15} className="text-accent" aria-hidden />
                    {folder.name}
                  </button>
                </td>
                <td className="tabular px-4 text-right text-data text-faint">—</td>
                <td className="px-4" />
                <td className="tabular px-4 text-data text-muted">
                  {relativeTime(folder.created_at)}
                </td>
                <td className="px-4 text-right">
                  <button
                    type="button"
                    aria-label={`Delete folder ${folder.name}`}
                    className="p-1 text-muted transition-colors duration-hover hover:text-danger"
                    onClick={() => setDeleting(folder)}
                  >
                    <Trash2 size={15} aria-hidden />
                  </button>
                </td>
              </tr>
            ))}

            {files.map((file) => (
              <tr
                key={file.id}
                data-row={file.id}
                aria-selected={file.id === selectedId}
                onClick={() => setSelectedId(file.id)}
                className={clsx(
                  'h-row cursor-pointer border-b border-line transition-colors duration-hover last:border-0 hover:bg-raised',
                  file.id === selectedId && 'bg-raised',
                )}
              >
                <td className="w-10 pl-4" onClick={(e) => e.stopPropagation()}>
                  <input
                    type="checkbox"
                    className="checkbox"
                    aria-label={`Select ${file.name}`}
                    checked={sel.isSelected(selection, file.id)}
                    onChange={(e) => {
                      // Shift extends from the anchor; a plain click toggles
                      // and moves the anchor here.
                      const native = e.nativeEvent as unknown as { shiftKey?: boolean };
                      setSelection((cur) =>
                        native.shiftKey
                          ? sel.selectRange(cur, file.id, visibleIds)
                          : sel.toggle(cur, file.id),
                      );
                    }}
                  />
                </td>
                <td className="max-w-0 truncate px-4 text-body text-text">
                  {/* A real button, so the row is reachable and activatable by
                      keyboard rather than click-only. */}
                  <button
                    type="button"
                    className="block w-full truncate text-left"
                    onClick={(e) => {
                      e.stopPropagation();
                      setSelectedId(file.id);
                    }}
                  >
                    {file.name}
                  </button>
                </td>
                <td className="tabular px-4 text-right text-data text-muted">
                  {bytes(file.size_bytes)}
                </td>
                <td className="px-4" onClick={(e) => e.stopPropagation()}>
                  <ShardMap file={file} drives={drives} />
                </td>
                <td className="tabular px-4 text-data text-muted">{relativeTime(file.created_at)}</td>
                <td className="px-4" onClick={(e) => e.stopPropagation()}>
                  <div className="flex items-center justify-end gap-1">
                    <button
                      type="button"
                      aria-label={`Download ${file.name}`}
                      className="p-1 text-muted transition-colors duration-hover hover:text-text"
                      onClick={() => void download(file)}
                    >
                      <Download size={15} aria-hidden />
                    </button>
                    <button
                      type="button"
                      aria-label={`Move ${file.name} to trash`}
                      className="p-1 text-muted transition-colors duration-hover hover:text-danger"
                      onClick={() => trash.mutate(file.id)}
                    >
                      <Trash2 size={15} aria-hidden />
                    </button>
                  </div>
                </td>
              </tr>
            ))}
          </tbody>
        </table>

        {/*
          Below 768px the table becomes a list of stacked rows.

          A horizontally scrolling table is not an answer on a phone: the
          columns that get pushed off are Size, Stored and Added, which is
          every column except the one you already knew. Stacking keeps all of
          them, and the shard chips stay on the first line because "which
          drives is this on" is the question this product exists to answer.
        */}
        <ul className="divide-y divide-line md:hidden">
          {children.map((folder) => (
            <li key={folder.id} className="flex items-center gap-3 px-4 py-3">
              <button
                type="button"
                className="flex min-w-0 flex-1 items-center gap-2 text-left"
                onClick={() => setFolderId(folder.id)}
              >
                <FolderIcon size={16} className="shrink-0 text-accent" aria-hidden />
                <span className="truncate text-body text-text">{folder.name}</span>
              </button>
              <span className="tabular shrink-0 text-caption text-faint">
                {relativeTime(folder.created_at)}
              </span>
              <button
                type="button"
                aria-label={`Delete folder ${folder.name}`}
                className="shrink-0 rounded p-2 text-muted hover:text-danger"
                onClick={() => setDeleting(folder)}
              >
                <Trash2 size={16} aria-hidden />
              </button>
            </li>
          ))}

          {files.map((file) => (
            <li key={file.id} data-row={file.id} className="px-4 py-3">
              <div className="flex items-start gap-3">
                <div className="min-w-0 flex-1">
                  {/* Only the name is the button: the metadata row below holds
                      the shard map, whose trigger is itself a button, and
                      nesting one inside another is invalid. */}
                  <button
                    type="button"
                    className="block w-full truncate text-left text-body text-text"
                    onClick={() => setSelectedId(file.id)}
                  >
                    {file.name}
                  </button>
                  <div className="mt-1.5 flex flex-wrap items-center gap-x-2 gap-y-1">
                    <ShardMap file={file} drives={drives} />
                    <span className="tabular text-caption text-muted">
                      {bytes(file.size_bytes)}
                    </span>
                    <span aria-hidden className="text-caption text-faint">·</span>
                    <span className="tabular text-caption text-faint">
                      {relativeTime(file.created_at)}
                    </span>
                  </div>
                </div>
                <div className="flex shrink-0 items-center gap-1">
                  <button
                    type="button"
                    aria-label={`Download ${file.name}`}
                    className="rounded p-2 text-muted hover:text-text"
                    onClick={() => void download(file)}
                  >
                    <Download size={16} aria-hidden />
                  </button>
                  <button
                    type="button"
                    aria-label={`Move ${file.name} to trash`}
                    className="rounded p-2 text-muted hover:text-danger"
                    onClick={() => trash.mutate(file.id)}
                  >
                    <Trash2 size={16} aria-hidden />
                  </button>
                </div>
              </div>
            </li>
          ))}
        </ul>

        {/* Design.md §7: an empty state states the next action, not the
            absence. The drop zone *is* the empty state rather than sitting
            above it — one target instead of two. */}
        {!isLoading && files.length === 0 && children.length === 0 && !listError && (
          <div className="p-4">
            <button
              type="button"
              onClick={() => inputRef.current?.click()}
              className="flex w-full flex-col items-center gap-2 rounded-lg border border-dashed
                         border-border px-6 py-14 text-center transition-colors duration-hover
                         hover:border-accent hover:bg-raised"
            >
              <Upload size={22} className="text-faint" aria-hidden />
              <span className="text-body font-semibold text-text">
                Drop files here to upload
              </span>
              <span className="text-body text-muted">
                Or press <kbd className="tabular text-data text-text">U</kbd>. Everything is
                encrypted before it leaves this machine.
              </span>
            </button>
          </div>
        )}

        {isLoading && <div className="px-4 py-16 text-center text-body text-muted">Loading…</div>}

        {listError && (
          <div className="px-4 py-16 text-center">
            <p className="text-body text-danger">
              {listError instanceof ApiError ? listError.message : 'Could not load your files.'}
            </p>
          </div>
        )}
      </div>

      {/*
        The drawer overlays rather than reflowing. Giving the listing a right
        padding would move every row sideways the instant one was clicked —
        the same displacement that made the old popover flicker (#27), just
        with a bigger box.

        Kept mounted while anything is selected so that picking another row
        swaps its contents instead of tearing it down and rebuilding it.
      */}
      {selected && (
        <FileDetail
          file={selected}
          drives={drives}
          open={selectedId !== null}
          onClose={closeDrawer}
          onDownload={(f) => void download(f)}
          onTrash={(f) => {
            trash.mutate(f.id);
            closeDrawer();
          }}
        />
      )}
    </div>
  );
}

function buildBreadcrumb(folders: Folder[], id: string | null): Folder[] {
  const trail: Folder[] = [];
  let cursor = id;
  // Bounded: a cycle in the tree would otherwise hang the render.
  for (let i = 0; cursor && i < 32; i++) {
    const folder = folders.find((f) => f.id === cursor);
    if (!folder) break;
    trail.unshift(folder);
    cursor = folder.parent_id;
  }
  return trail;
}
