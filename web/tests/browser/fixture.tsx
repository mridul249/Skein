/**
 * The render fixture the browser harness drives.
 *
 * It mounts the real components against fixed data, so a test never depends on
 * a server, a database or a connected Drive account. The numbers are chosen to
 * match the cases the tests assert on — notably two drives at 277/400 GB and
 * 8.1/15 GB, and a file with an orphaned shard.
 *
 * `?route=` selects which page to mount.
 */
import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { MemoryRouter, Route, Routes } from 'react-router-dom';

import '../../src/styles/index.css';
import { Layout } from '../../src/components/Layout';
import { Files } from '../../src/pages/Files';
import { Trash } from '../../src/pages/Trash';
import { Drives } from '../../src/pages/Drives';
import { Login } from '../../src/pages/Login';
import { UploadsProvider } from '../../src/lib/uploads-context';
import type { Drive, FileItem, Folder, Quota } from '../../src/lib/api';

const GB = 1024 ** 3;
const now = new Date().toISOString();
const ago = (days: number) => new Date(Date.now() - days * 86_400_000).toISOString();

function drive(ordinal: number, email: string, total: number, used: number): Drive {
  return {
    id: `drive-${ordinal}`,
    kind: 'gdrive',
    email,
    display_name: email,
    status: 'active',
    ordinal,
    created_at: now,
    total_bytes: total,
    used_bytes: used,
    reserved_bytes: 0,
    free_bytes: total - used,
    last_synced_at: now,
  };
}

const drives: Drive[] = [
  drive(1, 'mridul.kumar.longish.address@gmail.com', 400 * GB, 277 * GB),
  drive(2, 'backup1@gmail.com', 15 * GB, Math.round(8.1 * GB)),
];

const quota: Quota = {
  total_bytes: drives.reduce((s, d) => s + d.total_bytes, 0),
  used_bytes: drives.reduce((s, d) => s + d.used_bytes, 0),
  free_bytes: drives.reduce((s, d) => s + d.free_bytes, 0),
  drives,
};

const files: FileItem[] = [
  {
    id: 'f1',
    name: 'archive.tar.zst',
    folder_id: null,
    size_bytes: Math.round(28.4 * GB),
    is_striped: true,
    is_encrypted: true,
    status: 'ready',
    sha256: 'a3f21c00000000000000000000000000000000000000000000000000000c9e1c',
    created_at: ago(2),
    updated_at: ago(2),
    // Shard 2 is orphaned: account_id null, the state a disconnect leaves
    // behind today (known issue #19). It must not borrow drive 1's colour.
    shards: [0, 1, 2].map((i) => ({
      index: i,
      account_id: i === 2 ? null : `drive-${i + 1}`,
      size_bytes: Math.round(9.5 * GB),
      plain_size_bytes: Math.round(9.46 * GB),
      plain_offset: Math.round(i * 9.46 * GB),
    })),
  },
  {
    id: 'f2',
    name: 'a-deliberately-very-long-file-name-to-test-truncation-behaviour.mkv',
    folder_id: null,
    size_bytes: Math.round(4.1 * GB),
    is_striped: false,
    is_encrypted: true,
    status: 'ready',
    created_at: ago(5),
    updated_at: ago(5),
    shards: [
      {
        index: 0,
        account_id: 'drive-1',
        size_bytes: Math.round(4.1 * GB),
        plain_size_bytes: Math.round(4.1 * GB),
        plain_offset: 0,
      },
    ],
  },
];

const folders: Folder[] = [{ id: 'fo1', parent_id: null, name: 'projects', created_at: ago(30) }];

const client = new QueryClient({
  defaultOptions: { queries: { staleTime: Infinity, retry: false, refetchOnMount: false } },
});
client.setQueryData(['quota'], quota);
client.setQueryData(['drives'], { accounts: drives });
client.setQueryData(['folders'], { folders });
client.setQueryData(['files', null], { files });
client.setQueryData(['trash'], {
  files: files.map((f) => ({ ...f, deleted_at: ago(1) })),
});

const route = new URLSearchParams(window.location.search).get('route') ?? '/';

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <QueryClientProvider client={client}>
      <UploadsProvider>
        <MemoryRouter initialEntries={[route]}>
          <Routes>
            <Route path="/login" element={<Login />} />
            <Route element={<Layout />}>
              <Route index element={<Files />} />
              <Route path="trash" element={<Trash />} />
              <Route path="settings" element={<Drives />} />
            </Route>
          </Routes>
        </MemoryRouter>
      </UploadsProvider>
    </QueryClientProvider>
  </StrictMode>,
);
