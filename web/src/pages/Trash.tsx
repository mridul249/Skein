import { useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { RotateCcw, Trash2 } from 'lucide-react';

import { ApiError, api } from '../lib/api';
import { bytes, relativeTime } from '../lib/format';

/**
 * Trash. Nothing here has been deleted at the provider — trashing is
 * reversible by design, so the bytes stay put until someone says otherwise.
 */
export function Trash() {
  const qc = useQueryClient();
  const [banner, setBanner] = useState('');

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

  return (
    <div>
      <header className="mb-5">
        <h1 className="font-display text-display-m font-semibold tracking-tight">Trash</h1>
        <p className="mt-1 text-body text-subtext0">
          Nothing here has been removed from your drives yet.
        </p>
      </header>

      {banner && (
        <p role="alert" className="mb-4 rounded border border-red/40 bg-red/10 px-3 py-2 text-body text-red">
          {banner}
        </p>
      )}

      <div className="card overflow-x-auto">
        <table className="w-full min-w-[36rem] border-collapse">
          <thead>
            <tr className="border-b border-surface0 text-left">
              <th scope="col" className="px-4 py-2.5 text-label font-medium text-subtext0">
                Name
              </th>
              <th scope="col" className="w-28 px-4 py-2.5 text-right text-label font-medium text-subtext0">
                Size
              </th>
              <th scope="col" className="w-28 px-4 py-2.5 text-label font-medium text-subtext0">
                Trashed
              </th>
              <th scope="col" className="w-28 px-4 py-2.5">
                <span className="sr-only">Actions</span>
              </th>
            </tr>
          </thead>
          <tbody>
            {files.map((file) => (
              <tr key={file.id} className="h-row border-b border-surface0/60 hover:bg-surface0">
                <td className="max-w-0 truncate px-4 text-body">{file.name}</td>
                <td className="tabular px-4 text-right text-data text-subtext0">
                  {bytes(file.size_bytes)}
                </td>
                <td className="px-4 text-data text-overlay0">
                  {file.deleted_at ? relativeTime(file.deleted_at) : '—'}
                </td>
                <td className="px-4">
                  <div className="flex items-center justify-end gap-1">
                    <button
                      type="button"
                      aria-label={`Restore ${file.name}`}
                      className="rounded p-1 text-overlay0 transition-colors duration-hover hover:text-green"
                      onClick={() => restore.mutate(file.id)}
                    >
                      <RotateCcw size={15} aria-hidden />
                    </button>
                    <button
                      type="button"
                      aria-label={`Delete ${file.name} permanently`}
                      className="rounded p-1 text-overlay0 transition-colors duration-hover hover:text-red"
                      onClick={() => {
                        const shards = file.shards.length;
                        const ok = window.confirm(
                          `Delete ${file.name} and its ${shards} ${shards === 1 ? 'shard' : 'shards'}? This cannot be undone.`,
                        );
                        if (ok) erase.mutate(file.id);
                      }}
                    >
                      <Trash2 size={15} aria-hidden />
                    </button>
                  </div>
                </td>
              </tr>
            ))}
          </tbody>
        </table>

        {isLoading && <div className="px-4 py-16 text-center text-body text-overlay0">Loading…</div>}

        {!isLoading && files.length === 0 && (
          <div className="px-4 py-16 text-center">
            <p className="font-display text-heading text-subtext0">Trash is empty.</p>
          </div>
        )}
      </div>
    </div>
  );
}
