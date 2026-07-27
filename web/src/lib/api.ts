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
export class ApiError extends Error {
  readonly status: number;
  readonly code: string;
  readonly requestId?: string;
  readonly fields?: Record<string, string>;

  constructor(
    status: number,
    code: string,
    message: string,
    requestId?: string,
    fields?: Record<string, string>,
  ) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
    this.code = code;
    this.requestId = requestId;
    this.fields = fields;
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
    };
    if (res.status === 401) clearSession();
    throw new ApiError(
      res.status,
      e.error ?? 'error',
      e.message ?? 'Something went wrong.',
      e.request_id,
      e.fields,
    );
  }

  return payload as T;
}

export const api = {
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
  upload(
    file: File,
    folderId: string | null,
    onProgress: (fraction: number) => void,
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
          if (e.lengthComputable && e.total > 0) onProgress(e.loaded / e.total);
        });

        xhr.addEventListener('load', () => {
          if (xhr.status >= 200 && xhr.status < 300) {
            onProgress(1);
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
   * Fetches file content as a blob URL for preview or download.
   *
   * The URL cannot simply be an <a href> because the access token lives in
   * memory and an anchor cannot carry an Authorization header.
   */
  async contentURL(id: string): Promise<string> {
    const token = await ensureToken();
    if (!token) throw new ApiError(401, 'unauthorized', 'Sign in to continue.');

    const res = await fetch(`/api/files/${encodeURIComponent(id)}/content`, {
      headers: { Authorization: `Bearer ${token}` },
      credentials: 'same-origin',
    });
    if (!res.ok) throw new ApiError(res.status, 'download_failed', 'Could not download that file.');
    return URL.createObjectURL(await res.blob());
  },
};
