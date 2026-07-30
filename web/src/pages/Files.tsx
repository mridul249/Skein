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
import { bytes, relativeTime } from '../lib/format';
import { useUploads } from '../lib/uploads-context';

export function Files() {
  const qc = useQueryClient();
  const [folderId, setFolderId] = useState<string | null>(null);
  const [dragging, setDragging] = useState(false);
  const [banner, setBanner] = useState('');
  const inputRef = useRef<HTMLInputElement>(null);
  const { start: startUploadJob } = useUploads();

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
      }
    }
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, []);

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

  /**
   * Hands the transfer to the browser rather than running it in the tab.
   *
   * Only the short capability URL crosses JS; the bytes never do. The browser
   * streams straight to disk, shows the transfer in its own download manager,
   * and keeps going if this tab closes — which is also why no beforeunload
   * warning belongs on this path, and why there is no progress bar to build.
   */
  async function download(file: FileItem) {
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
      setBanner(err instanceof ApiError ? err.message : 'Could not download that file.');
    }
  }

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
        'min-h-[60vh] rounded transition-colors duration-hover',
        dragging && 'outline-dashed outline-2 outline-offset-4 outline-mauve',
      )}
    >
      <header className="mb-5 flex flex-wrap items-center justify-between gap-3">
        <h1 className="font-display text-display-m font-semibold tracking-tight">Files</h1>
        <div className="flex gap-2">
          <button
            type="button"
            className="btn-ghost"
            onClick={() => {
              const name = window.prompt('Folder name');
              if (name) newFolder.mutate(name);
            }}
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
        <p role="alert" className="mb-4 rounded border border-red/40 bg-red/10 px-3 py-2 text-body text-red">
          {banner}
        </p>
      )}

      {breadcrumb.length > 0 && (
        <nav aria-label="Breadcrumb" className="mb-3 flex items-center gap-1 text-label">
          <button type="button" className="text-sapphire" onClick={() => setFolderId(null)}>
            All files
          </button>
          {breadcrumb.map((f) => (
            <span key={f.id} className="flex items-center gap-1">
              <ChevronRight size={13} className="text-overlay0" aria-hidden />
              <button type="button" className="text-sapphire" onClick={() => setFolderId(f.id)}>
                {f.name}
              </button>
            </span>
          ))}
        </nav>
      )}

      <div className="card overflow-x-auto">
        <table className="w-full min-w-[40rem] border-collapse">
          <thead>
            <tr className="border-b border-surface0 text-left">
              <th scope="col" className="px-4 py-2.5 text-label font-medium text-subtext0">
                Name
              </th>
              <th scope="col" className="w-28 px-4 py-2.5 text-right text-label font-medium text-subtext0">
                Size
              </th>
              <th scope="col" className="w-28 px-4 py-2.5 text-label font-medium text-subtext0">
                Stored
              </th>
              <th scope="col" className="w-24 px-4 py-2.5 text-label font-medium text-subtext0">
                Added
              </th>
              <th scope="col" className="w-24 px-4 py-2.5">
                <span className="sr-only">Actions</span>
              </th>
            </tr>
          </thead>
          <tbody>
            {children.map((folder) => (
              <tr
                key={folder.id}
                className="h-row border-b border-surface0/60 transition-colors duration-hover hover:bg-surface0"
              >
                <td className="px-4">
                  <button
                    type="button"
                    className="flex items-center gap-2 text-body text-text"
                    onClick={() => setFolderId(folder.id)}
                  >
                    <FolderIcon size={15} className="text-mauve" aria-hidden />
                    {folder.name}
                  </button>
                </td>
                <td className="px-4 text-right text-data text-overlay0">—</td>
                <td className="px-4" />
                <td className="px-4 text-data text-overlay0">
                  {relativeTime(folder.created_at)}
                </td>
                <td className="px-4 text-right">
                  <button
                    type="button"
                    aria-label={`Delete folder ${folder.name}`}
                    className="rounded p-1 text-overlay0 transition-colors duration-hover hover:text-red"
                    onClick={() => {
                      if (window.confirm(`Delete ${folder.name} and everything in it?`)) {
                        void api.deleteFolder(folder.id).then(refresh);
                      }
                    }}
                  >
                    <Trash2 size={15} aria-hidden />
                  </button>
                </td>
              </tr>
            ))}

            {files.map((file) => (
              <tr
                key={file.id}
                className="h-row border-b border-surface0/60 transition-colors duration-hover hover:bg-surface0"
              >
                <td className="max-w-0 truncate px-4 text-body">{file.name}</td>
                <td className="tabular px-4 text-right text-data text-subtext0">
                  {bytes(file.size_bytes)}
                </td>
                <td className="px-4">
                  <ShardMap file={file} drives={drives} />
                </td>
                <td className="px-4 text-data text-overlay0">{relativeTime(file.created_at)}</td>
                <td className="px-4">
                  <div className="flex items-center justify-end gap-1">
                    <button
                      type="button"
                      aria-label={`Download ${file.name}`}
                      className="rounded p-1 text-overlay0 transition-colors duration-hover hover:text-text"
                      onClick={() => void download(file)}
                    >
                      <Download size={15} aria-hidden />
                    </button>
                    <button
                      type="button"
                      aria-label={`Move ${file.name} to trash`}
                      className="rounded p-1 text-overlay0 transition-colors duration-hover hover:text-red"
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

        {!isLoading && files.length === 0 && children.length === 0 && (
          <div className="px-4 py-16 text-center">
            <p className="font-display text-heading text-subtext0">Nothing here yet.</p>
            <p className="mt-1 text-body text-overlay0">Drag a file in, or press U.</p>
          </div>
        )}

        {isLoading && <div className="px-4 py-16 text-center text-body text-overlay0">Loading…</div>}

        {listError && (
          <div className="px-4 py-16 text-center text-body text-red">
            {listError instanceof ApiError ? listError.message : 'Could not load your files.'}
          </div>
        )}
      </div>
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
