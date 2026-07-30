import { useState, type FormEvent } from 'react';
import { useNavigate } from 'react-router-dom';

import { ApiError, api } from '../lib/api';
import { useSession } from '../lib/session';
import { Wordmark } from '../components/Wordmark';

/**
 * Sign in and sign up on one screen.
 *
 * Design.md §7: errors say what happened and what to do. A field error from
 * the server is shown against its field rather than collapsed into one banner.
 */
export function Login() {
  const navigate = useNavigate();
  const { setUser } = useSession();

  const [mode, setMode] = useState<'login' | 'register'>('login');
  const [email, setEmail] = useState('');
  const [password, setPassword] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [fields, setFields] = useState<Record<string, string>>({});

  async function submit(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError('');
    setFields({});

    try {
      const user =
        mode === 'login' ? await api.login(email, password) : await api.register(email, password);
      setUser(user);
      navigate('/', { replace: true });
    } catch (err) {
      if (err instanceof ApiError) {
        setError(err.message);
        setFields(err.fields ?? {});
      } else {
        setError('Could not reach the server.');
      }
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="flex min-h-screen items-center justify-center px-5">
      <div className="w-full max-w-sm">
        <div className="mb-8">
          <h1 className="flex items-center gap-1ch">
            <img src="/mark.svg" width="28" height="28" alt="" aria-hidden />
            <Wordmark className="text-display-m" />
          </h1>
          <p className="mt-1 text-body text-subtext0">
            Your free cloud accounts, pooled, striped and encrypted.
          </p>
        </div>

        <form onSubmit={(e) => void submit(e)} className="card space-y-1line p-2ch" noValidate>
          <div>
            <label htmlFor="email" className="label">
              Email
            </label>
            <input
              id="email"
              type="email"
              autoComplete="email"
              required
              value={email}
              onChange={(e) => setEmail(e.target.value)}
              className="input"
              aria-invalid={Boolean(fields.email)}
              aria-describedby={fields.email ? 'email-error' : undefined}
            />
            {fields.email && (
              <p id="email-error" className="mt-1 text-caption text-red">
                {fields.email}
              </p>
            )}
          </div>

          <div>
            <label htmlFor="password" className="label">
              Password
            </label>
            <input
              id="password"
              type="password"
              autoComplete={mode === 'login' ? 'current-password' : 'new-password'}
              required
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              className="input"
              aria-invalid={Boolean(fields.password)}
              aria-describedby={fields.password ? 'password-error' : 'password-hint'}
            />
            {fields.password ? (
              <p id="password-error" className="mt-1 text-caption text-red">
                {fields.password}
              </p>
            ) : (
              mode === 'register' && (
                <p id="password-hint" className="mt-1 text-caption text-subtext0">
                  At least 12 characters. Length is what matters, not symbols.
                </p>
              )
            )}
          </div>

          {error && !Object.keys(fields).length && (
            <p role="alert" className="text-caption text-red">
              {error}
            </p>
          )}

          <button type="submit" disabled={busy} className="btn-primary w-full">
            {busy ? 'Working…' : mode === 'login' ? 'Sign in' : 'Create account'}
          </button>
        </form>

        <p className="mt-4 text-center text-caption text-subtext0">
          {mode === 'login' ? 'No account yet?' : 'Already have an account?'}{' '}
          <button
            type="button"
            className="text-sapphire underline underline-offset-2"
            onClick={() => {
              setMode(mode === 'login' ? 'register' : 'login');
              setError('');
              setFields({});
            }}
          >
            {mode === 'login' ? 'Create one' : 'Sign in'}
          </button>
        </p>
      </div>
    </div>
  );
}
