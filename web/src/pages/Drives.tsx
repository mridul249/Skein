import { useEffect, useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Plus, RefreshCw, Unlink } from 'lucide-react';
import clsx from 'clsx';

import { ApiError, api, type Drive } from '../lib/api';
import { DRIVE_BG, bytes, driveColor, percent, usageTone } from '../lib/format';
import { Modal } from '../components/Modal';
import { AccountChip } from '../components/AccountChip';

/** Drives: connect, sync and disconnect the accounts that hold the bytes. */
export function Drives() {
  const qc = useQueryClient();
  const [banner, setBanner] = useState('');
  const [tone, setTone] = useState<'error' | 'ok'>('ok');
  /** The drive awaiting a disconnect confirmation, if any. */
  const [confirming, setConfirming] = useState<Drive | null>(null);

  const { data, isLoading } = useQuery({ queryKey: ['quota'], queryFn: api.quota });

  // The OAuth callback redirects back here with the outcome in the query
  // string. It is read once and then stripped, so a refresh does not replay
  // a stale message.
  useEffect(() => {
    const params = new URLSearchParams(window.location.search);
    const status = params.get('drive');
    if (!status) return;

    if (status === 'connected') {
      setTone('ok');
      setBanner('Drive connected.');
      void qc.invalidateQueries({ queryKey: ['quota'] });
      void qc.invalidateQueries({ queryKey: ['drives'] });
    } else {
      setTone('error');
      setBanner(params.get('message') ?? 'Could not connect that drive.');
    }
    window.history.replaceState({}, '', window.location.pathname);
  }, [qc]);

  const connect = useMutation({
    mutationFn: api.connectGoogle,
    onSuccess: (res) => {
      // A full navigation, not a popup: consent screens break in popups
      // that a browser has decided are unwanted.
      window.location.href = res.authorize_url;
    },
    onError: (err: unknown) => {
      setTone('error');
      setBanner(err instanceof ApiError ? err.message : 'Could not start the connection.');
    },
  });

  const sync = useMutation({
    mutationFn: (id: string) => api.syncDrive(id),
    onSuccess: () => void qc.invalidateQueries({ queryKey: ['quota'] }),
    onError: (err: unknown) => {
      setTone('error');
      setBanner(err instanceof ApiError ? err.message : 'Could not refresh that drive.');
    },
  });

  const disconnect = useMutation({
    mutationFn: (id: string) => api.disconnectDrive(id),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ['quota'] });
      void qc.invalidateQueries({ queryKey: ['drives'] });
    },
    onError: (err: unknown) => {
      setTone('error');
      setBanner(err instanceof ApiError ? err.message : 'Could not disconnect that drive.');
    },
  });

  const drives = data?.drives ?? [];

  return (
    <div>
      {/*
        Design.md §7: name the consequence, not "Are you sure?".

        The wording below is load-bearing and is reproduced verbatim from the
        `window.confirm` this replaced. It used to end "until you reconnect",
        which is false: disconnect deletes the account row, so the shards' link
        to it is nulled and reconnecting mints a new row that re-links nothing.
        Known issue #19. The mechanism fix is scheduled for Session 3 (soft
        delete, keeping the row id stable); until it lands the warning has to
        say what actually happens. Restore the reassuring wording when the fix
        makes it true — and not before.
      */}
      <Modal
        open={confirming !== null}
        title={confirming ? `Disconnect ${confirming.email}?` : ''}
        intent="danger"
        confirmLabel="Disconnect"
        onCancel={() => setConfirming(null)}
        onConfirm={() => {
          if (confirming) disconnect.mutate(confirming.id);
          setConfirming(null);
        }}
      >
        Files with a shard on it become unreadable, and reconnecting does NOT
        currently restore access — the link between those files and this drive is
        lost. Nothing is deleted from Google; the data is still there, but Skein
        will not be able to find it.
      </Modal>

      <header className="mb-5 flex flex-wrap items-center justify-between gap-3">
        {/* Capped in characters, not pixels. At a monospace body face this
            sentence is ~85 cells wide and pushed the primary action onto its
            own row at 1280px; wrapping the prose instead keeps the action
            where Design.md §4 puts it. */}
        <div className="max-w-prose">
          <h1 className="text-title font-semibold text-text">Drives</h1>
          <p className="mt-1 text-body text-muted">
            Skein sees only the files it created. It cannot read anything already in your Drive.
          </p>
        </div>
        <button
          type="button"
          className="btn-primary"
          disabled={connect.isPending}
          onClick={() => connect.mutate()}
        >
          <Plus size={15} aria-hidden />
          Connect Google Drive
        </button>
      </header>

      {banner && (
        <p
          role="status"
          className={clsx(
            'mb-4 rounded-md border px-4 py-3 text-body',
            tone === 'error' ? 'border-danger/40 bg-danger/10 text-danger' : 'border-success/40 bg-success/10 text-success',
          )}
        >
          {banner}
        </p>
      )}

      {data && drives.length > 0 && (
        <div className="card mb-5 px-4 py-3.5">
          <p className="tabular text-data text-muted">
            {bytes(data.used_bytes)} used of {bytes(data.total_bytes)} pooled ·{' '}
            {bytes(data.free_bytes)} free
          </p>
        </div>
      )}

      {isLoading && <p className="text-body text-muted">Loading…</p>}

      {!isLoading && drives.length === 0 && (
        <div className="card px-4 py-16 text-center">
          <p className="text-body font-semibold text-text">No drives connected yet.</p>
          <p className="mt-1 text-body text-muted">
            Connect a Google account to give Skein somewhere to put your files.
          </p>
        </div>
      )}

      <ul className="space-y-3">
        {drives.map((drive) => {
          const pct = percent(drive.used_bytes, drive.total_bytes);
          const usage = usageTone(drive.used_bytes, drive.total_bytes);
          return (
            <li key={drive.id} className="card p-4 transition-colors duration-hover hover:border-borderStrong">
              <div className="flex flex-wrap items-start justify-between gap-3">
                <div className="flex min-w-0 items-center gap-3">
                  <AccountChip ordinal={drive.ordinal} />
                  <div className="min-w-0">
                    <p className="truncate text-body text-text">{drive.email}</p>
                    <p className="tabular text-data-sm text-muted">
                      {drive.kind === 'gdrive' ? 'Google Drive' : drive.kind}
                      {drive.last_synced_at && ` · synced ${relative(drive.last_synced_at)}`}
                    </p>
                  </div>
                </div>

                <div className="flex shrink-0 gap-1">
                  <button
                    type="button"
                    aria-label={`Refresh quota for ${drive.email}`}
                    className="p-2 text-muted transition-colors duration-hover hover:text-text"
                    disabled={sync.isPending}
                    onClick={() => sync.mutate(drive.id)}
                  >
                    <RefreshCw size={15} aria-hidden />
                  </button>
                  <button
                    type="button"
                    aria-label={`Disconnect ${drive.email}`}
                    className="p-2 text-muted transition-colors duration-hover hover:text-danger"
                    onClick={() => setConfirming(drive)}
                  >
                    <Unlink size={15} aria-hidden />
                  </button>
                </div>
              </div>

              <div className="mt-3">
                <div className="mb-1 flex items-baseline justify-between">
                  <span className="tabular text-data-sm text-muted">
                    {bytes(drive.used_bytes)} / {bytes(drive.total_bytes)}
                    {drive.reserved_bytes > 0 && ` · ${bytes(drive.reserved_bytes)} reserved`}
                  </span>
                  <span
                    className={clsx(
                      'tabular text-data-sm',
                      usage === 'red' && 'text-danger',
                      usage === 'yellow' && 'text-warning',
                      usage === 'green' && 'text-muted',
                    )}
                  >
                    {pct}%
                  </span>
                </div>
                <div className="h-1.5 w-full overflow-hidden rounded-full bg-raised">
                  <div
                    className={clsx('h-full rounded-full', DRIVE_BG[driveColor(drive.ordinal)])}
                    style={{ width: `${pct}%` }}
                  />
                </div>
              </div>

              {drive.status !== 'active' && (
                <p className="mt-2 text-caption text-warning">
                  {drive.last_error || 'This drive needs attention.'}
                </p>
              )}
            </li>
          );
        })}
      </ul>
    </div>
  );
}

function relative(iso: string): string {
  const then = new Date(iso).getTime();
  if (Number.isNaN(then)) return 'never';
  const minutes = Math.floor((Date.now() - then) / 60000);
  if (minutes < 1) return 'just now';
  if (minutes < 60) return `${minutes}m ago`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours}h ago`;
  return `${Math.floor(hours / 24)}d ago`;
}
