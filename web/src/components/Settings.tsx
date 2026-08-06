import { useEffect, useId, useRef, useState } from 'react';
import clsx from 'clsx';

import { ApiError, api } from '../lib/api';
import { RecoveryPanel } from './RecoveryPanel';

type Tab = 'general' | 'accounts' | 'security' | 'recovery';

const TABS: { id: Tab; label: string }[] = [
  { id: 'general', label: 'General' },
  { id: 'accounts', label: 'Accounts' },
  { id: 'security', label: 'Security' },
  { id: 'recovery', label: 'Recovery' },
];

interface SettingsProps {
  open: boolean;
  onClose: () => void;
  email: string;
  driveCount: number;
  onManageDrives: () => void;
}

/**
 * The settings dialog.
 *
 * Its own component rather than the shared Modal: Modal is a confirm dialog
 * with one optional text field, and forcing a three-tab panel through it would
 * make both harder to read.
 */
export function Settings({ open, onClose, email, driveCount, onManageDrives }: SettingsProps) {
  const [tab, setTab] = useState<Tab>('general');
  const dialogRef = useRef<HTMLDivElement>(null);

  // Escape closes, matching Modal.
  useEffect(() => {
    if (!open) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose();
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [open, onClose]);

  // Reset to the first tab between openings, so reopening does not land on
  // whatever was last touched.
  useEffect(() => {
    if (open) setTab('general');
  }, [open]);

  if (!open) return null;

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/50 p-4"
      role="presentation"
      onMouseDown={(e) => {
        if (e.target === e.currentTarget) onClose();
      }}
    >
      <div
        ref={dialogRef}
        role="dialog"
        aria-modal="true"
        aria-label="Settings"
        className="card w-full max-w-lg overflow-hidden"
      >
        <div className="flex items-center justify-between border-b border-border px-5 py-4">
          <h2 className="text-label font-semibold text-text">Settings</h2>
          <button
            type="button"
            onClick={onClose}
            aria-label="Close settings"
            className="rounded px-2 py-1 text-caption text-muted
                       transition-colors duration-hover hover:bg-raised hover:text-text"
          >
            Close
          </button>
        </div>

        <div role="tablist" aria-label="Settings sections" className="flex gap-1 border-b border-border px-3">
          {TABS.map((t) => (
            <button
              key={t.id}
              role="tab"
              type="button"
              id={`settings-tab-${t.id}`}
              aria-selected={tab === t.id}
              aria-controls={`settings-panel-${t.id}`}
              onClick={() => setTab(t.id)}
              className={clsx(
                'border-b-2 px-3 py-2 text-body transition-colors duration-hover',
                tab === t.id
                  ? 'border-accent text-text'
                  : 'border-transparent text-muted hover:text-text',
              )}
            >
              {t.label}
            </button>
          ))}
        </div>

        <div className="max-h-[60vh] overflow-y-auto p-5">
          {tab === 'general' && <GeneralPanel email={email} />}
          {tab === 'accounts' && (
            <AccountsPanel driveCount={driveCount} onManageDrives={onManageDrives} />
          )}
          {tab === 'security' && <SecurityPanel />}
          {tab === 'recovery' && <RecoveryPanel />}
        </div>
      </div>
    </div>
  );
}

function Panel({ id, children }: { id: Tab; children: React.ReactNode }) {
  return (
    <div role="tabpanel" id={`settings-panel-${id}`} aria-labelledby={`settings-tab-${id}`}>
      {children}
    </div>
  );
}

export function GeneralPanel({ email }: { email: string }) {
  return (
    <Panel id="general">
      <dl className="space-y-3">
        <div>
          <dt className="text-caption text-muted">Signed in as</dt>
          <dd className="text-body text-text">{email}</dd>
        </div>
        <div>
          <dt className="text-caption text-muted">Encryption</dt>
          <dd className="text-body text-text">
            Files are encrypted on this machine before they leave it.
          </dd>
        </div>
      </dl>
    </Panel>
  );
}

function AccountsPanel({
  driveCount,
  onManageDrives,
}: {
  driveCount: number;
  onManageDrives: () => void;
}) {
  return (
    <Panel id="accounts">
      <p className="mb-4 text-body text-text">
        {driveCount === 0
          ? 'No drives are connected yet.'
          : `${driveCount} ${driveCount === 1 ? 'drive is' : 'drives are'} connected.`}
      </p>
      <button type="button" className="btn-ghost" onClick={onManageDrives}>
        Manage drives
      </button>
    </Panel>
  );
}

/** A password field with a show/hide toggle. */
function PasswordField({
  label,
  value,
  onChange,
  autoComplete,
  invalid,
  describedBy,
}: {
  label: string;
  value: string;
  onChange: (v: string) => void;
  autoComplete: string;
  invalid?: boolean;
  describedBy?: string;
}) {
  const id = useId();
  const [shown, setShown] = useState(false);

  return (
    <div>
      <label htmlFor={id} className="mb-1 block text-caption text-muted">
        {label}
      </label>
      <div className="relative">
        <input
          id={id}
          type={shown ? 'text' : 'password'}
          className="input w-full pr-16"
          value={value}
          autoComplete={autoComplete}
          aria-invalid={invalid || undefined}
          aria-describedby={describedBy}
          onChange={(e) => onChange(e.target.value)}
        />
        <button
          type="button"
          onClick={() => setShown((s) => !s)}
          // The control's purpose changes with its state, so the label has to
          // as well — a static "Toggle password" leaves a screen reader user
          // unable to tell which state they are in.
          aria-label={shown ? `Hide ${label.toLowerCase()}` : `Show ${label.toLowerCase()}`}
          aria-pressed={shown}
          className="absolute inset-y-0 right-0 px-3 text-caption text-muted
                     transition-colors duration-hover hover:text-text"
        >
          {shown ? 'Hide' : 'Show'}
        </button>
      </div>
    </div>
  );
}

export function SecurityPanel() {
  const [current, setCurrent] = useState('');
  const [next, setNext] = useState('');
  const [confirm, setConfirm] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [fieldError, setFieldError] = useState<string | null>(null);
  const [currentError, setCurrentError] = useState<string | null>(null);
  const [done, setDone] = useState(false);

  // Mismatch is decided here, not by the server: it is a property of the two
  // boxes on this screen and the server has no opinion about it. Strength and
  // "is that really your current password" are the server's calls.
  const mismatch = confirm.length > 0 && next !== confirm;
  const canSubmit = !busy && current.length > 0 && next.length > 0 && !mismatch;

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    if (!canSubmit) return;

    setBusy(true);
    setError(null);
    setFieldError(null);
    setCurrentError(null);
    setDone(false);
    try {
      await api.changePassword(current, next);
      setCurrent('');
      setNext('');
      setConfirm('');
      setDone(true);
    } catch (err) {
      if (err instanceof ApiError) {
        // Surfaced as-is. The server owns the strength rule (12 characters,
        // no composition requirements) and its message is the one that
        // matches what it actually enforced.
        //
        // current_password is rendered against its own field rather than as a
        // banner: a wrong current password is a typo in one box, and the user
        // should be looking at that box. The server returns it as a field
        // error precisely so this is possible — it used to return 401, which
        // signed the user out before any of this could render.
        setCurrentError(err.fields?.current_password ?? null);
        setFieldError(err.fields?.password ?? null);
        const handled = err.fields?.current_password || err.fields?.password;
        setError(handled ? null : err.message);
      } else {
        setError('Could not change your password.');
      }
    } finally {
      setBusy(false);
    }
  }

  return (
    <Panel id="security">
      <form onSubmit={submit} className="space-y-4">
        <PasswordField
          label="Current password"
          value={current}
          onChange={setCurrent}
          autoComplete="current-password"
          invalid={Boolean(currentError)}
          describedBy={currentError ? 'current-password-error' : undefined}
        />
        {currentError && (
          <p id="current-password-error" className="text-caption text-danger">
            {currentError}
          </p>
        )}
        <PasswordField
          label="New password"
          value={next}
          onChange={setNext}
          autoComplete="new-password"
          invalid={Boolean(fieldError)}
          describedBy={fieldError ? 'new-password-error' : undefined}
        />
        {fieldError && (
          <p id="new-password-error" className="text-caption text-danger">
            {fieldError}
          </p>
        )}
        <PasswordField
          label="Confirm new password"
          value={confirm}
          onChange={setConfirm}
          autoComplete="new-password"
          invalid={mismatch}
          describedBy={mismatch ? 'confirm-error' : undefined}
        />
        {mismatch && (
          <p id="confirm-error" className="text-caption text-danger">
            Those do not match.
          </p>
        )}

        {/*
          THE HONEST STATEMENT, and it is not decoration. This now promises
          revocation, so it is only true while the per-user epoch (known issue
          #18) is actually wired to ChangePassword. If that ever regresses,
          this text becomes the dangerous kind of wrong — a user believing they
          locked out a stolen session when they did not. It is pinned by
          TestPasswordChangeRevokesOtherSessions and by the concurrent case
          beside it.
        */}
        <p className="rounded border border-border bg-raised p-3 text-caption text-muted">
          Your other devices will be signed out. This one stays signed in.
        </p>

        {error && (
          <p role="alert" className="text-caption text-danger">
            {error}
          </p>
        )}
        {done && (
          <p role="status" className="text-caption text-success">
            Password changed. Your other devices have been signed out.
          </p>
        )}

        <button type="submit" className="btn-primary" disabled={!canSubmit}>
          {busy ? 'Changing…' : 'Change password'}
        </button>
      </form>
    </Panel>
  );
}
