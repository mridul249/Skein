import { useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { RotateCcw, Trash2 } from 'lucide-react';

import { ApiError, api, type FileItem } from '../lib/api';
import { bytes, relativeTime } from '../lib/format';
import { Modal } from '../components/Modal';

/**
 * Trash. Nothing here has been deleted at the provider — trashing is
 * reversible by design, so the bytes stay put until someone says otherwise.
 */
export function Trash() {
  const qc = useQueryClient();
  const [banner, setBanner] = useState('');
  /** The file awaiting a permanent-delete confirmation, if any. */
  const [erasing, setErasing] = useState<FileItem | null>(null);

  const { data, isLoading } = useQuery({ queryKey: ['trash'], queryFn: api.listTrash });

  const refresh = () => {
    void qc.invalidateQueries({ queryKey: ['trash'] });
    void qc.invalidateQueries({ queryKey: ['files'] });
    void qc.invalidateQueries({ queryKey: ['quota'] });
  };

  const restore = useMutation({
    mutationFn: (id: string) => api.restoreFile(id),
    onSuccess: refresh,
    onError: (err: unknown) =>
      setBanner(err instanceof ApiError ? err.message : 'Could not restore that file.'),
  });

  const erase = useMutation({
    mutationFn: (id: string) => api.deleteFileForever(id),
    onSuccess: refresh,
    onError: (err: unknown) =>
      setBanner(err instanceof ApiError ? err.message : 'Could not delete that file.'),
  });

  const files = data?.files ?? [];
  const shardCount = erasing?.shards.length ?? 0;

  return (
    <div>
      {/* Design.md §7: "Delete archive.tar.zst and its 3 shards?", never
          "Are you sure?". The count is the whole point — it is the only place
          the user learns the file was striped. */}
      <Modal
        open={erasing !== null}
        title={
          erasing
            ? `Delete ${erasing.name} and its ${shardCount} ${shardCount === 1 ? 'shard' : 'shards'}?`
            : ''
        }
        intent="danger"
        confirmLabel="Delete forever"
        onCancel={() => setErasing(null)}
        onConfirm={() => {
          if (erasing) erase.mutate(erasing.id);
          setErasing(null);
        }}
      >
        This cannot be undone.
      </Modal>

      <header className="mb-5">
        <h1 className="text-title font-semibold">Trash</h1>
        <p className="mt-1 text-body text-muted">
          Nothing here has been removed from your drives yet.
        </p>
      </header>

      {banner && (
        <p role="alert" className="mb-4 border border-danger/40 bg-danger/10 px-4 py-2 text-body text-danger">
          {banner}
        </p>
      )}

      {/*
        `relative` is load-bearing. Without it the card is not the containing
        block for its absolutely-positioned descendants, so the `sr-only`
        spans in the table header escape `overflow-x-auto` entirely and extend
        the *document's* scrollable width — the whole page scrolled sideways
        by 305px at 375px while the table looked correctly clipped.
      */}
      <div className="card relative overflow-x-auto">
        <table className="w-full min-w-[42rem] border-collapse">
          <thead>
            <tr className="border-b border-border text-left">
              <th scope="col" className="px-4 py-2 text-label font-semibold text-muted">
                Name
              </th>
              <th scope="col" className="w-28 px-4 py-2 text-right text-label font-semibold text-muted">
                Size
              </th>
              <th scope="col" className="w-28 px-4 py-2 text-label font-semibold text-muted">
                Trashed
              </th>
              <th scope="col" className="w-28 px-4 py-2">
                <span className="sr-only">Actions</span>
              </th>
            </tr>
          </thead>
          <tbody>
            {files.map((file) => (
              <tr key={file.id} className="h-row border-b border-border/60 hover:bg-raised">
                <td className="max-w-0 truncate px-4 text-body">{file.name}</td>
                <td className="tabular px-4 text-right text-data text-muted">
                  {bytes(file.size_bytes)}
                </td>
                <td className="px-4 text-data text-muted">
                  {file.deleted_at ? relativeTime(file.deleted_at) : '—'}
                </td>
                <td className="px-4">
                  <div className="flex items-center justify-end gap-1">
                    <button
                      type="button"
                      aria-label={`Restore ${file.name}`}
                      className="p-1 text-muted transition-colors duration-hover hover:text-success"
                      onClick={() => restore.mutate(file.id)}
                    >
                      <RotateCcw size={15} aria-hidden />
                    </button>
                    <button
                      type="button"
                      aria-label={`Delete ${file.name} permanently`}
                      className="p-1 text-muted transition-colors duration-hover hover:text-danger"
                      onClick={() => setErasing(file)}
                    >
                      <Trash2 size={15} aria-hidden />
                    </button>
                  </div>
                </td>
              </tr>
            ))}
          </tbody>
        </table>

        {isLoading && <div className="px-4 py-16 text-center text-body text-muted">Loading…</div>}

        {!isLoading && files.length === 0 && (
          <div className="px-4 py-16 text-center">
            <p className="text-heading text-muted">Trash is empty.</p>
          </div>
        )}
      </div>
    </div>
  );
}
