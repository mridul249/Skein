/**
 * Account health, in THREE states rather than two.
 *
 * This is where the server's refresh-failure classification has to survive
 * into the UI. Block 3b split two failures that both mean "Drive is unusable"
 * but need opposite responses:
 *
 *   needs_reauth          invalid_grant, 409, carries account_id.
 *                         The user revoked access or the token expired.
 *                         RECONNECTING FIXES IT.
 *
 *   provider_misconfigured  unauthorized_client, 503, NO account_id.
 *                         The OAuth client itself is wrong. The account is
 *                         healthy. RECONNECTING CAN NEVER SUCCEED — an
 *                         operator has to fix server configuration.
 *
 * Collapsing them into one "something's wrong, reconnect" state rebuilds
 * exactly the loop 3b was written to prevent: the user reconnects, the
 * exchange fails identically, and they go round again with nothing they can
 * do about it. So a misconfigured client renders as a SERVER-LEVEL BANNER
 * with no Reconnect button, and never as a badge on a drive.
 */
import clsx from 'clsx';

export type DriveStatus = 'active' | 'needs_reauth' | 'disabled' | string;

/** The amber badge on a drive pill. Only ever for a per-account fault. */
export function DriveStatusBadge({
  status,
  onReconnect,
  busy,
}: {
  status: DriveStatus;
  onReconnect?: () => void;
  busy?: boolean;
}) {
  if (status === 'active') return null;

  if (status === 'needs_reauth') {
    return (
      <span className="inline-flex items-center gap-2">
        <span
          className="rounded-full border border-warning px-1.5 py-0.5 text-caption text-warning"
          // The colour is not the only carrier: the word says it too, which
          // is what makes this legible under colour-vision deficiency.
          title="This drive needs to be reconnected"
        >
          Needs reconnect
        </span>
        {onReconnect && (
          <button
            type="button"
            onClick={onReconnect}
            disabled={busy}
            className="rounded px-1.5 py-0.5 text-caption text-accent
                       transition-colors duration-hover hover:bg-raised
                       disabled:cursor-not-allowed disabled:opacity-45"
          >
            {busy ? 'Opening…' : 'Reconnect'}
          </button>
        )}
      </span>
    );
  }

  return <span className="text-caption text-muted">{status}</span>;
}

/**
 * The server-configuration banner.
 *
 * Deliberately NOT attached to a drive and deliberately carries no Reconnect
 * button. The drives are fine; the client credentials are not.
 */
export function ProviderMisconfiguredBanner({
  message,
  className,
}: {
  message?: string;
  className?: string;
}) {
  return (
    <div
      role="alert"
      className={clsx(
        'rounded border border-danger bg-raised p-3 text-body text-text',
        className,
      )}
    >
      <p className="font-semibold">Google sign-in is misconfigured on this server</p>
      <p className="mt-1 text-caption text-muted">
        {message ??
          "Skein's Google client credentials are missing or wrong, so drive access cannot be refreshed."}{' '}
        This is a server setting — reconnecting a drive will not fix it. On the
        desktop app, set{' '}
        <code className="text-text">SKEIN_GOOGLE_DESKTOP_CLIENT_ID</code> and{' '}
        <code className="text-text">SKEIN_GOOGLE_DESKTOP_CLIENT_SECRET</code>; on a
        server, set <code className="text-text">SKEIN_GOOGLE_CLIENT_ID</code> and{' '}
        <code className="text-text">SKEIN_GOOGLE_CLIENT_SECRET</code>.
      </p>
    </div>
  );
}
