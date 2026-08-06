import { useCallback, useEffect, useState } from 'react';
import clsx from 'clsx';

import {
  ApiError,
  type BackfillReport,
  type ReconstructReport,
  type RecoveryStatus,
  api,
} from '../lib/api';
import {
  coverageSummary,
  coverageVerdict,
  restoreSummary,
  tallyBackfill,
  unscannedAccounts,
} from '../lib/recovery';

/**
 * The Recovery section of Settings.
 *
 * TWO QUESTIONS, IN THE ORDER SOMEONE ACTUALLY ASKS THEM:
 *
 *  1. "If I lost the database, could I get my files back?" — manifest
 *     coverage, with a Backfill action when the answer is no.
 *  2. "I lost it. Get them back." — Restore from Drives.
 *
 * THERE IS NO FIELD THAT ACCEPTS A MASTER KEY, and there must never be. The key
 * has to be in the environment before startup: key_id verification refuses to
 * boot on a mismatch, and every subsystem derives from the keyring during
 * wiring. A paste-your-key box would be taking it after everything that needs
 * it has already run — it could not work, and offering it would teach the user
 * that recovery starts here rather than in their .env.
 */
export function RecoveryPanel() {
  const [status, setStatus] = useState<RecoveryStatus | null>(null);
  const [coverage, setCoverage] = useState<BackfillReport | null>(null);
  const [busy, setBusy] = useState<'' | 'coverage' | 'backfill' | 'rewrite' | 'scan' | 'apply'>('');
  const [error, setError] = useState('');

  // The operator token, held only in component state for the length of one
  // action. Never persisted: web storage is forbidden in this bundle
  // (httpapi.TestBundleDoesNotUseWebStorage) and a backup token in
  // localStorage would be a second standing credential.
  const [token, setToken] = useState('');

  const [backfillReport, setBackfillReport] = useState<BackfillReport | null>(null);
  const [scan, setScan] = useState<ReconstructReport | null>(null);
  const [applied, setApplied] = useState<ReconstructReport | null>(null);

  const loadCoverage = useCallback(async () => {
    setBusy('coverage');
    setError('');
    try {
      setCoverage(await api.manifestCoverage());
    } catch (err) {
      setError(err instanceof ApiError ? err.message : 'Could not check your drives.');
    } finally {
      setBusy('');
    }
  }, []);

  useEffect(() => {
    void api
      .recoveryStatus()
      .then(setStatus)
      .catch(() => setStatus(null));
    void loadCoverage();
  }, [loadCoverage]);

  const verdict = coverageVerdict(coverage);

  async function runBackfill(rewrite = false) {
    setBusy(rewrite ? 'rewrite' : 'backfill');
    setError('');
    try {
      const report = await api.backfillManifests(token, rewrite);
      setBackfillReport(report);
      setCoverage(report);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : 'Backfill failed.');
    } finally {
      setBusy('');
    }
  }

  // STEP ONE. Scanning writes nothing; it exists so the user sees what would
  // happen before anything happens.
  async function runScan() {
    setBusy('scan');
    setError('');
    setApplied(null);
    try {
      setScan(await api.reconstruct(true));
    } catch (err) {
      setError(err instanceof ApiError ? err.message : 'The scan failed.');
    } finally {
      setBusy('');
    }
  }

  // STEP TWO, and only reachable from a scan the user has read.
  async function runApply() {
    setBusy('apply');
    setError('');
    try {
      const report = await api.reconstruct(false);
      setApplied(report);
      setScan(null);
      void loadCoverage();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : 'The restore failed.');
    } finally {
      setBusy('');
    }
  }

  return (
    <div className="space-y-6">
      {error && (
        <p role="alert" className="text-caption text-danger">
          {error}
        </p>
      )}

      {/* ---- The key. Stated first, because nothing below works without it. ---- */}
      <section data-recovery-key className="rounded border border-border bg-raised p-3">
        <h3 className="text-label font-semibold text-muted">Master key</h3>
        <p className="mt-1 text-body text-text">
          This instance is running under key{' '}
          <span className="tabular font-semibold" data-key-id>
            {status?.key_id || 'unknown'}
          </span>
        </p>
        <p className="mt-2 text-caption text-muted">
          Recovery starts by putting the right key in your <code>.env</code> as{' '}
          <code>SKEIN_MASTER_KEY</code>, before starting Skein. Skein refuses to start if
          the key does not match this database, so a wrong key fails immediately rather
          than part-way through a restore. Compare the ID above with the{' '}
          <code>Key ID:</code> line in your exported key file — see{' '}
          <code>docs/BACKUP.md</code>.
        </p>
        <p className="mt-2 text-caption text-muted">
          Skein never asks you to paste a key here: by the time this screen renders, the
          key has already been used to open your files.
        </p>
      </section>

      {/* ---- Coverage. ---- */}
      <section data-recovery-coverage>
        <div className="flex items-baseline justify-between gap-3">
          <h3 className="text-label font-semibold text-muted">Manifest coverage</h3>
          <button
            type="button"
            className="btn-quiet text-caption"
            onClick={() => void loadCoverage()}
            disabled={busy !== ''}
          >
            {busy === 'coverage' ? 'Checking…' : 'Re-check'}
          </button>
        </div>

        <p
          data-coverage-verdict={verdict}
          className={clsx(
            'mt-1 text-body',
            verdict === 'full' && 'text-success',
            verdict === 'partial' && 'text-warning',
            verdict === 'none' && 'text-danger',
            verdict === 'unknown' && 'text-muted',
          )}
        >
          {coverageSummary(coverage)}
        </p>

        {coverage && unscannedAccounts(coverage).length > 0 && (
          <ul className="mt-2 space-y-1 text-caption text-warning">
            {unscannedAccounts(coverage).map((reason, i) => (
              <li key={i}>Drive not checked: {reason}</li>
            ))}
          </ul>
        )}

        {coverage && coverage.coverage.damaged > 0 && (
          <p className="mt-2 text-caption text-muted">
            {coverage.coverage.damaged} damaged{' '}
            {coverage.coverage.damaged === 1 ? 'file is' : 'files are'} excluded: a
            manifest cannot recover a file whose shards are already gone.
          </p>
        )}

        <div className="mt-3 space-y-2">
          {!status?.manifests_writable ? (
            <p className="text-caption text-muted">
              Writing manifests needs <code>SKEIN_BACKUP_TOKEN</code> set on the
              server. It is the same operator token the database backup uses.
            </p>
          ) : (
            <>
              {/* The token field appears only when the server actually wants
                  one. On desktop the person clicking IS the operator, so
                  asking for a token would be a field nobody can fill in. */}
              {!status.manifests_writable_without_token && (
                <label className="block text-caption text-muted">
                  Operator token
                  <input
                    type="password"
                    className="input mt-1 w-full"
                    value={token}
                    onChange={(e) => setToken(e.target.value)}
                    autoComplete="off"
                    placeholder="SKEIN_BACKUP_TOKEN"
                  />
                </label>
              )}

              <div className="flex flex-wrap gap-2">
                {verdict !== 'full' && (
                  <button
                    type="button"
                    className="btn-primary"
                    disabled={busy !== '' || (!status.manifests_writable_without_token && token === '')}
                    onClick={() => void runBackfill(false)}
                    data-backfill
                  >
                    {busy === 'backfill' ? 'Writing manifests…' : 'Write missing manifests'}
                  </button>
                )}

                {/*
                  REPAIR IS OFFERED EVEN AT FULL COVERAGE, and that is the
                  point of it. A manifest written by an older version can be
                  present and complete while missing a field added later — so
                  coverage reads 100% and the library is still unrecoverable
                  after a database rebuild. Ordinary backfill skips those files
                  precisely because a manifest is already there.
                */}
                <button
                  type="button"
                  className={verdict === 'full' ? 'btn-primary' : 'btn-ghost'}
                  disabled={busy !== '' || (!status.manifests_writable_without_token && token === '')}
                  onClick={() => void runBackfill(true)}
                  data-rewrite
                >
                  {busy === 'rewrite' ? 'Repairing…' : 'Repair all manifests'}
                </button>
              </div>

              <p className="text-caption text-muted">
                Repair rewrites every manifest from the database. Run it once after
                updating Skein, so files uploaded by an older version can still be
                recovered if you lose the database.
              </p>
            </>
          )}
        </div>

        {backfillReport && (
          <div className="mt-3 rounded border border-line p-3 text-caption">
            <p className="text-text">Backfill results</p>
            <ul className="mt-1 space-y-0.5 text-muted">
              {Object.entries(tallyBackfill(backfillReport)).map(([state, n]) => (
                <li key={state}>
                  {n} {state.replace(/_/g, ' ')}
                </li>
              ))}
            </ul>
            {!backfillReport.complete && (
              <p className="mt-2 text-warning">
                Some drives could not be reached, so this did not cover everything. Running
                it again is safe.
              </p>
            )}
          </div>
        )}
      </section>

      {/* ---- Restore. Two steps, never one. ---- */}
      <section data-recovery-restore>
        <h3 className="text-label font-semibold text-muted">Restore from drives</h3>
        <p className="mt-1 text-caption text-muted">
          Rebuilds the file list from the manifests on your drives. It only adds what is
          missing and never changes or removes anything already here, so it is safe to
          run against a working library.
        </p>

        {!scan && !applied && (
          <button
            type="button"
            className="btn-ghost mt-3"
            disabled={busy !== ''}
            onClick={() => void runScan()}
            data-restore-scan
          >
            {busy === 'scan' ? 'Scanning…' : 'Scan drives'}
          </button>
        )}

        {/*
          THE TWO-STEP GATE. The scan result is shown, and only then is applying
          offered. A single button that mutates the database is the wrong shape
          for an operation you run when things have already gone wrong — the
          user should see what was found before anything is written.
        */}
        {scan && !applied && (
          <div className="mt-3 space-y-2 rounded border border-line p-3">
            <p className="text-body text-text">Found on your drives</p>
            <ul className="space-y-0.5 text-caption text-muted">
              <li>{scan.manifests_found} manifests</li>
              <li>{scan.files_recovered} files added</li>
              <li>{scan.shards_recovered} shard records added</li>
              <li>{scan.folders_recovered} folders recreated</li>
              <li>{scan.files_already_present} files already present</li>
            </ul>
            {unscannedAccounts(scan).length > 0 && (
              <ul className="space-y-0.5 text-caption text-warning">
                {unscannedAccounts(scan).map((reason, i) => (
                  <li key={i}>Drive not scanned: {reason}</li>
                ))}
              </ul>
            )}
            <p className="text-caption text-muted">
              Nothing has been written yet. This is what restoring would add.
            </p>
            <div className="flex gap-2 pt-1">
              <button
                type="button"
                className="btn-primary"
                data-restore-apply
                disabled={busy !== ''}
                onClick={() => void runApply()}
              >
                {busy === 'apply' ? 'Restoring…' : 'Restore these files'}
              </button>
              <button type="button" className="btn-quiet" onClick={() => setScan(null)}>
                Cancel
              </button>
            </div>
          </div>
        )}

        {applied && (
          <div className="mt-3 space-y-1 rounded border border-line p-3" data-restore-result>
            {restoreSummary(applied).map((line, i) => (
              <p
                key={i}
                className={clsx(
                  'text-caption',
                  /incomplete/.test(line) ? 'text-warning' : 'text-muted',
                )}
              >
                {line}
              </p>
            ))}
            <button
              type="button"
              className="btn-quiet mt-1"
              onClick={() => {
                setApplied(null);
                setScan(null);
                void loadCoverage();
              }}
            >
              Close
            </button>
          </div>
        )}
      </section>
    </div>
  );
}
