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
import { useEffect, useRef } from 'react';
import { createRoot } from 'react-dom/client';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { MemoryRouter, Route, Routes } from 'react-router-dom';

import '../../src/styles/index.css';
import { Layout } from '../../src/components/Layout';
import { Files } from '../../src/pages/Files';
import { Trash } from '../../src/pages/Trash';
import { Drives } from '../../src/pages/Drives';
import { Login } from '../../src/pages/Login';
import { UploadsProvider, useUploads } from '../../src/lib/uploads-context';
import { DownloadsProvider } from '../../src/lib/downloads-context';
import {
  api,
  __setAccessTokenForTests,
  type Drive,
  type FileItem,
  type Folder,
  type Quota,
} from '../../src/lib/api';

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

const params = new URLSearchParams(window.location.search);

if (params.has('preview')) {
  // Fixture-only patch. `contentURL` normally mints a signed capability URL
  // from the server; here it returns a static asset so the preview element
  // has something real to fetch. What is under test is the element and the
  // request it issues, not the minting.
  api.contentURL = async (id: string) =>
    id === 'f-photo' ? './assets/photo.png' : './assets/clip.mp4';
}

if (params.has('desktop')) {
  // Makes the capability probe answer "desktop build", so the drawer swap in
  // Layout can be observed for real.
  //
  // Stubbed at window.fetch rather than at the module: what is under test is
  // the whole chain — probe -> `desktop` -> which component Layout renders —
  // and stubbing probeDesktop itself would skip the first two links. A session
  // is installed for the same reason: authedFetch refreshes before it calls
  // fetch, and without a token it never issues the request at all. That is
  // precisely the bug this fixture mode exists to pin.
  // `latelogin` reproduces the REAL sequence from the running app: the
  // provider mounts with no session, the probe fires and is refused, and the
  // login lands afterwards. Without this the fixture installs a token before
  // mount and the probe never sees the unauthenticated state — which is
  // exactly why the browser test passed while the desktop drawer stayed dead.
  const lateLogin = params.has('latelogin');
  if (!lateLogin) {
    __setAccessTokenForTests('fixture-token');
  } else {
    __setAccessTokenForTests(null);
    // Sign in shortly after mount, as a user would.
    setTimeout(
      () =>
        __setAccessTokenForTests('fixture-token', {
          id: 'u1',
          email: 'fixture@example.com',
          email_verified: true,
          created_at: now,
        }),
      150,
    );
  }
  const realFetch = window.fetch.bind(window);
  window.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(typeof input === 'string' || input instanceof URL ? input : input.url);
    if (url.includes('/api/desktop/capabilities')) {
      if (new Headers(init?.headers).get('Authorization') !== 'Bearer fixture-token') {
        // Exactly what the real server does, and what shipped: an
        // unauthenticated probe reads as "not the desktop build".
        return new Response(JSON.stringify({ error: 'unauthorized' }), { status: 401 });
      }
      return new Response(
        JSON.stringify({ desktop_downloads: true, download_dir: '/home/x/Downloads' }),
        { status: 200, headers: { 'Content-Type': 'application/json' } },
      );
    }
    if (url.includes('/api/desktop/downloads')) {
      return new Response(JSON.stringify({ downloads: [] }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      });
    }
    return realFetch(input as RequestInfo, init);
  }) as typeof window.fetch;
}

const client = new QueryClient({
  defaultOptions: { queries: { staleTime: Infinity, retry: false, refetchOnMount: false } },
});
client.setQueryData(['quota'], quota);
client.setQueryData(['drives'], { accounts: drives });
client.setQueryData(['folders'], { folders });
const previewFiles: FileItem[] = [
  {
    id: 'f-photo',
    name: 'photo.png',
    folder_id: null,
    size_bytes: 344,
    is_striped: false,
    is_encrypted: true,
    status: 'ready',
    created_at: ago(1),
    updated_at: ago(1),
    shards: [
      { index: 0, account_id: 'drive-1', size_bytes: 400, plain_size_bytes: 344, plain_offset: 0 },
    ],
  },
  {
    id: 'f-clip',
    name: 'clip.mp4',
    folder_id: null,
    size_bytes: 2 * 1024 * 1024,
    is_striped: true,
    is_encrypted: true,
    status: 'ready',
    created_at: ago(2),
    updated_at: ago(2),
    shards: [
      { index: 0, account_id: 'drive-1', size_bytes: 1050000, plain_size_bytes: 1048576, plain_offset: 0 },
      { index: 1, account_id: 'drive-2', size_bytes: 1050000, plain_size_bytes: 1048576, plain_offset: 1048576 },
    ],
  },
];

client.setQueryData(['files', null], {
  files: params.has('preview') ? previewFiles : files,
});
client.setQueryData(['trash'], {
  files: files.map((f) => ({ ...f, deleted_at: ago(1) })),
});

const route = params.get('route') ?? '/';

/**
 * Stubs api.bulkDelete so partial failure can be exercised for real.
 *
 * Stubbed at the api object rather than at window.fetch: request() refreshes
 * the access token BEFORE it calls fetch, and this fixture has no session, so
 * a fetch-level stub is never reached — every call fails with "Sign in to
 * continue" instead. Everything above the api boundary (the toolbar, the
 * chunking, the outcome rendering, the failed files staying selected) is the
 * real code under test.
 *
 * `?bulkfail=N` fails the first N files of each request.
 */
if (params.has('bulkfail')) {
  const failCount = Number(params.get('bulkfail') ?? '0');
  (api as unknown as Record<string, unknown>).bulkDelete = async (fileIds: string[]) => {
    const results = fileIds.map((id, i) => ({
      file_id: id,
      ok: i >= failCount,
      code: i < failCount ? 'rate_limited' : undefined,
      error:
        i < failCount
          ? 'A drive is rate limiting. Try this file again shortly.'
          : undefined,
    }));
    return {
      results,
      succeeded: results.filter((r) => r.ok).length,
      failed: results.filter((r) => !r.ok).length,
    };
  };
}

/**
 * A stub uploader so the fixture can show real `sending` and `finishing`
 * states without a network.
 *
 * `finishing` is the state that matters: every byte has left the machine and
 * the server is still writing shards, so it is reached by reporting
 * sent === total and then never resolving. With the real uploader the only
 * state a fixture can reach is `error`.
 */
const stubUploader = (
  file: { name: string; size: number },
  _folderId: string | null,
  onProgress: (sent: number, total: number) => void,
) => {
  if (file.name.startsWith('finishing')) {
    onProgress(file.size, file.size);
  } else {
    onProgress(Math.round(file.size * 0.4), file.size);
  }
  // Never settles by default: both jobs stay on screen for the length of the
  // test. `?settle` exposes a resolver so a test can drive them to a terminal
  // state and exercise dismiss / clear-all for real.
  return new Promise<void>((resolve, reject) => {
    settlers.push({ name: file.name, resolve, reject });
  });
};

/**
 * Handles for the seeded uploads, so a test can finish them on demand.
 *
 * Exposed on window rather than through a test-only prop: the point is to
 * drive the REAL store through the REAL component, not a parallel harness.
 */
const settlers: { name: string; resolve: () => void; reject: (e: Error) => void }[] = [];
(window as unknown as Record<string, unknown>).__settleUpload = (
  name: string,
  outcome: 'done' | 'error' = 'done',
) => {
  const s = settlers.find((x) => x.name.startsWith(name));
  if (!s) return false;
  if (outcome === 'done') s.resolve();
  else s.reject(new Error('A drive refused the upload.'));
  return true;
};

function SeedTransfers() {
  const { start } = useUploads();
  const seeded = useRef(false);
  useEffect(() => {
    // StrictMode invokes effects twice in development, which seeded every
    // job twice and made the panel show four transfers instead of two.
    if (seeded.current) return;
    seeded.current = true;
    start({ name: 'finishing-archive.tar.zst', size: 28 * GB } as never, null);
    start({ name: 'sending-video.mkv', size: 4 * GB } as never, null);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);
  return null;
}

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <QueryClientProvider client={client}>
      <UploadsProvider uploader={params.has('transfers') ? stubUploader : undefined}>
        <DownloadsProvider>
          {params.has('transfers') && <SeedTransfers />}
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
        </DownloadsProvider>
      </UploadsProvider>
    </QueryClientProvider>
  </StrictMode>,
);
