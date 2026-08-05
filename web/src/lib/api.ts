/**
 * The API client.
 *
 * Rules.md §2.15 is the whole design constraint here: the access token lives
 * in a module-scoped variable and nowhere else. There is no localStorage, no
 * sessionStorage, no cookie the page can read. The refresh token is in an
 * httpOnly cookie the server set, which this code cannot see and does not
 * need to — it just calls /api/auth/refresh and the browser attaches it.
 *
 * The cost is that a page reload starts with no access token. That is fine:
 * bootstrap() calls refresh once on load, and a token that survives a reload
 * is a token sitting in storage where any injected script can read it.
 */

let accessToken: string | null = null;
let expiresAt = 0;

/** Listeners fire when the session appears or disappears. */
type SessionListener = (user: User | null) => void;
const listeners = new Set<SessionListener>();

export interface User {
  id: string;
  email: string;
  email_verified: boolean;
  created_at: string;
}

export interface SessionResponse {
  access_token: string;
  expires_at: string;
  expires_in: number;
  user: User;
}

export interface Shard {
  index: number;
  account_id: string | null;
  size_bytes: number;
  plain_size_bytes: number;
  plain_offset: number;
}

export interface FileItem {
  id: string;
  name: string;
  folder_id: string | null;
  size_bytes: number;
  is_striped: boolean;
  is_encrypted: boolean;
  status: string;
  sha256?: string;
  created_at: string;
  updated_at: string;
  deleted_at?: string;
  shards: Shard[];
}

export interface Folder {
  id: string;
  parent_id: string | null;
  name: string;
  created_at: string;
}

export interface Drive {
  id: string;
  kind: string;
  email: string;
  display_name: string;
  status: string;
  last_error?: string;
  ordinal: number;
  created_at: string;
  total_bytes: number;
  used_bytes: number;
  reserved_bytes: number;
  free_bytes: number;
  last_synced_at: string | null;
}

export interface Quota {
  total_bytes: number;
  used_bytes: number;
  free_bytes: number;
  drives: Drive[];
}

/** ApiError carries the server's typed error shape. */
/** One file's outcome in a bulk operation. */
export interface BulkResult {
  file_id: string;
  ok: boolean;
  code?: string;
  error?: string;
}

export interface BulkResponse {
  results: BulkResult[];
  succeeded: number;
  failed: number;
}

/**
 * The server caps one bulk request. Mirrored here so the client can chunk
 * rather than let a large selection fail opaquely.
 */
export const BULK_LIMIT = 200;

export class ApiError extends Error {
  readonly status: number;
  readonly code: string;
  readonly requestId?: string;
  readonly fields?: Record<string, string>;
  /**
   * Which connected account a drive_needs_reauth error is about.
   *
   * Present only on that code. provider_misconfigured deliberately omits it:
   * a broken OAuth client is not any one drive's fault, and badging a healthy
   * account would put a Reconnect button in front of a user who can never
   * succeed with it. See the server's httpx.ErrorBody.
   */
  readonly accountId?: string;
  /**
   * Set on a `file_shards_missing` error: the file whose shards are gone, and
   * which indexes are missing.
   *
   * The indexes are what let the UI say "shard 2 of 5 is missing" instead of
   * "something went wrong", and what makes it obvious that offering a download
   * would be pointless — the download fails identically.
   */
  readonly fileId?: string;
  readonly missingShards?: number[];

  constructor(
    status: number,
    code: string,
    message: string,
    requestId?: string,
    fields?: Record<string, string>,
    accountId?: string,
    fileId?: string,
    missingShards?: number[],
  ) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
    this.code = code;
    this.requestId = requestId;
    this.fields = fields;
    this.accountId = accountId;
    this.fileId = fileId;
    this.missingShards = missingShards;
  }

  /** True when the file is damaged: shards recorded but not present. */
  get isDamagedFile(): boolean {
    return this.code === 'file_shards_missing';
  }
}

export function onSessionChange(fn: SessionListener): () => void {
  listeners.add(fn);
  return () => listeners.delete(fn);
}

function announce(user: User | null) {
  for (const fn of listeners) fn(user);
}

function setSession(s: SessionResponse) {
  accessToken = s.access_token;
  // Refresh a little early so a request never races its own expiry.
  expiresAt = Date.now() + Math.max(0, s.expires_in - 30) * 1000;
  announce(s.user);
}

function clearSession() {
  accessToken = null;
  expiresAt = 0;
  announce(null);
}

/**
 * Test seam: installs an access token without a login round trip.
 *
 * The token still lives only in this module's scope (Rules.md §2.15); this
 * just sets it directly so a test can assert on what a request CARRIES.
 */
export function __setAccessTokenForTests(token: string | null): void {
  accessToken = token;
  expiresAt = token ? Date.now() + 60_000 : 0;
}

/** A single in-flight refresh, shared by every caller that needs one. */
let refreshInFlight: Promise<boolean> | null = null;

async function refresh(): Promise<boolean> {
  if (refreshInFlight) return refreshInFlight;

  refreshInFlight = (async () => {
    try {
      const res = await fetch('/api/auth/refresh', {
        method: 'POST',
        credentials: 'same-origin',
      });
      if (!res.ok) {
        clearSession();
        return false;
      }
      setSession((await res.json()) as SessionResponse);
      return true;
    } catch {
      clearSession();
      return false;
    } finally {
      refreshInFlight = null;
    }
  })();

  return refreshInFlight;
}

async function ensureToken(): Promise<string | null> {
  if (accessToken && Date.now() < expiresAt) return accessToken;
  const ok = await refresh();
  return ok ? accessToken : null;
}

interface RequestOptions {
  method?: string;
  body?: unknown;
  /** Skips the token refresh, for the auth endpoints themselves. */
  anonymous?: boolean;
  signal?: AbortSignal;
}

async function request<T>(path: string, opts: RequestOptions = {}): Promise<T> {
  const headers: Record<string, string> = {};
  let body: BodyInit | undefined;

  if (opts.body !== undefined) {
    headers['Content-Type'] = 'application/json';
    body = JSON.stringify(opts.body);
  }

  if (!opts.anonymous) {
    const token = await ensureToken();
    if (!token) throw new ApiError(401, 'unauthorized', 'Sign in to continue.');
    headers.Authorization = `Bearer ${token}`;
  }

  const send = () =>
    fetch(path, {
      method: opts.method ?? 'GET',
      headers,
      body,
      credentials: 'same-origin',
      signal: opts.signal,
    });

  let res = await send();

  // One retry after a refresh: the token may have expired between the check
  // and the request. A second 401 is a real one.
  if (res.status === 401 && !opts.anonymous) {
    if (await refresh()) {
      headers.Authorization = `Bearer ${accessToken}`;
      res = await send();
    }
  }

  if (res.status === 204) return undefined as T;

  const text = await res.text();
  const payload: unknown = text ? JSON.parse(text) : {};

  if (!res.ok) {
    const e = payload as {
      error?: string;
      message?: string;
      request_id?: string;
      fields?: Record<string, string>;
      account_id?: string;
      file_id?: string;
      missing_shard_indexes?: number[];
    };
    if (res.status === 401) clearSession();
    throw new ApiError(
      res.status,
      e.error ?? 'error',
      e.message ?? 'Something went wrong.',
      e.request_id,
      e.fields,
      e.account_id,
      e.file_id,
      e.missing_shard_indexes,
    );
  }

  return payload as T;
}

export const api = {
  /**
   * Changes the signed-in user's password.
   *
   * OTHER DEVICES ARE NOT SIGNED OUT. The server verifies, validates and
   * rehashes, but does not revoke other sessions — that needs the per-user
   * epoch from known issue #18, which is schema work. The Settings copy says
   * so plainly; do not add a spinner or a message implying revocation.
   */
  async changePassword(currentPassword: string, newPassword: string): Promise<void> {
    await request<void>('/api/auth/change-password', {
      method: 'POST',
      body: { current_password: currentPassword, new_password: newPassword },
    });
  },

  /**
   * Moves many files to the trash, returning one result per file.
   *
   * Trashes rather than destroys, matching the single-file delete. Permanent
   * removal is bulkPurge.
   */
  async bulkDelete(fileIds: string[]): Promise<BulkResponse> {
    return request<BulkResponse>('/api/files/bulk-delete', {
      method: 'POST',
      body: { file_ids: fileIds },
    });
  },

  /** Permanently deletes many files. The trash view's delete. */
  async bulkPurge(fileIds: string[]): Promise<BulkResponse> {
    return request<BulkResponse>('/api/files/bulk-delete?permanent=true', {
      method: 'POST',
      body: { file_ids: fileIds },
    });
  },

  /** Permanently deletes every trashed file. */
  async emptyTrash(): Promise<BulkResponse> {
    return request<BulkResponse>('/api/trash/empty', { method: 'POST' });
  },

  /** Called once on load. A failure just means nobody is signed in. */
  async bootstrap(): Promise<User | null> {
    const ok = await refresh();
    if (!ok) return null;
    try {
      return await request<User>('/api/auth/me');
    } catch {
      return null;
    }
  },

  async register(email: string, password: string): Promise<User> {
    const s = await request<SessionResponse>('/api/auth/register', {
      method: 'POST',
      body: { email, password },
      anonymous: true,
    });
    setSession(s);
    return s.user;
  },

  async login(email: string, password: string): Promise<User> {
    const s = await request<SessionResponse>('/api/auth/login', {
      method: 'POST',
      body: { email, password },
      anonymous: true,
    });
    setSession(s);
    return s.user;
  },

  async logout(): Promise<void> {
    try {
      await request<void>('/api/auth/logout', { method: 'POST', anonymous: true });
    } finally {
      clearSession();
    }
  },

  listFiles: (folderId: string | null) =>
    request<{ files: FileItem[]; next_cursor?: string }>(
      `/api/files${folderId ? `?folder_id=${encodeURIComponent(folderId)}` : ''}`,
    ),

  listFolders: () => request<{ folders: Folder[] }>('/api/folders'),

  createFolder: (name: string, parentId: string | null) =>
    request<Folder>('/api/folders', {
      method: 'POST',
      body: { name, parent_id: parentId },
    }),

  deleteFolder: (id: string) =>
    request<void>(`/api/folders/${encodeURIComponent(id)}`, { method: 'DELETE' }),

  renameFile: (id: string, name: string) =>
    request<FileItem>(`/api/files/${encodeURIComponent(id)}`, {
      method: 'PATCH',
      body: { name },
    }),

  moveFile: (id: string, folderId: string | null) =>
    request<FileItem>(`/api/files/${encodeURIComponent(id)}`, {
      method: 'PATCH',
      body: { folder_id: folderId },
    }),

  trashFile: (id: string) =>
    request<void>(`/api/files/${encodeURIComponent(id)}`, { method: 'DELETE' }),

  deleteFileForever: (id: string) =>
    request<void>(`/api/files/${encodeURIComponent(id)}?permanent=true`, { method: 'DELETE' }),

  restoreFile: (id: string) =>
    request<void>(`/api/files/${encodeURIComponent(id)}/restore`, { method: 'POST' }),

  listTrash: () => request<{ files: FileItem[] }>('/api/trash'),

  quota: () => request<Quota>('/api/quota'),

  listDrives: () => request<{ accounts: Drive[] }>('/api/accounts'),

  connectGoogle: () =>
    request<{ authorize_url: string }>('/api/accounts/google/connect', { method: 'POST' }),

  syncDrive: (id: string) =>
    request<Drive>(`/api/accounts/${encodeURIComponent(id)}/sync`, { method: 'POST' }),

  disconnectDrive: (id: string) =>
    request<void>(`/api/accounts/${encodeURIComponent(id)}`, { method: 'DELETE' }),

  /**
   * Uploads one file with progress.
   *
   * XMLHttpRequest rather than fetch: fetch has no upload progress event, and
   * a progress bar that jumps from 0 to 100 on a 30 GB file is worse than no
   * bar at all. The body is a FormData holding the File object itself, so the
   * browser streams it from disk — the page never holds the bytes either.
   */
  /**
   * onProgress reports bytes, not a fraction, and the bytes are those handed
   * to the browser's network stack — not bytes the server has accepted. The
   * caller is responsible for not presenting the two as the same thing; see
   * UploadJob.sent.
   */
  upload(
    file: File,
    folderId: string | null,
    onProgress: (sent: number, total: number) => void,
    signal?: AbortSignal,
  ): Promise<FileItem> {
    return new Promise((resolve, reject) => {
      void (async () => {
        const token = await ensureToken();
        if (!token) {
          reject(new ApiError(401, 'unauthorized', 'Sign in to continue.'));
          return;
        }

        const form = new FormData();
        // Order matters: the server needs every metadata field before the
        // first content byte, because a resumable session declares its
        // length up front.
        if (folderId) form.append('folder_id', folderId);
        form.append('size', String(file.size));
        form.append('name', file.name);
        form.append('file', file, file.name);

        const xhr = new XMLHttpRequest();
        xhr.open('POST', '/api/uploads');
        xhr.setRequestHeader('Authorization', `Bearer ${token}`);
        xhr.withCredentials = true;

        xhr.upload.addEventListener('progress', (e) => {
          if (e.lengthComputable && e.total > 0) onProgress(e.loaded, e.total);
        });

        xhr.addEventListener('load', () => {
          if (xhr.status >= 200 && xhr.status < 300) {
            onProgress(file.size, file.size);
            resolve(JSON.parse(xhr.responseText) as FileItem);
            return;
          }
          let message = 'Upload failed.';
          let code = 'error';
          try {
            const parsed = JSON.parse(xhr.responseText) as {
              message?: string;
              error?: string;
            };
            message = parsed.message ?? message;
            code = parsed.error ?? code;
          } catch {
            /* a non-JSON error body is still an error */
          }
          reject(new ApiError(xhr.status, code, message));
        });

        xhr.addEventListener('error', () =>
          reject(new ApiError(0, 'network', 'The connection dropped.')),
        );
        xhr.addEventListener('abort', () =>
          reject(new ApiError(0, 'aborted', 'Upload cancelled.')),
        );

        signal?.addEventListener('abort', () => xhr.abort());
        xhr.send(form);
      })();
    });
  },

  /**
   * Mints a short-lived capability URL the browser can fetch on its own.
   *
   * This replaces reading the response into a Blob. A Blob URL required the
   * entire file to be materialised in the tab before a single byte reached
   * disk, which undid the constant-memory guarantee the server upholds all
   * the way to the last hop (known issue #15). Nothing here touches the
   * bytes: the returned URL carries its own signature, so an <a download>
   * authenticates without an Authorization header the anchor cannot set.
   *
   * The returned URL is a credential. It is scoped to this one file and this
   * one user and expires in minutes, but it should not be logged, copied into
   * a bug report, or persisted anywhere.
   */
  async contentURL(id: string): Promise<string> {
    const res = await request<{ url: string; expires_at: string }>(
      `/api/files/${encodeURIComponent(id)}/content-url`,
      { method: 'POST' },
    );
    return res.url;
  },
};
