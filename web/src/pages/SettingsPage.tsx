import { useEffect, useState } from 'react';
import { useSearchParams } from 'react-router-dom';
import clsx from 'clsx';

import { useSession } from '../lib/session';
import { GeneralPanel, SecurityPanel } from '../components/Settings';
import { RecoveryPanel } from '../components/RecoveryPanel';
import { Drives } from './Drives';

/**
 * Settings, as a route rather than a dialog.
 *
 * IT USED TO BE A MODAL MOUNTED INSIDE THE DRIVES PAGE, which put the
 * containing relationship exactly backwards: `/settings` rendered `<Drives />`,
 * and Settings opened on top of it with an "Accounts" tab whose only content
 * was a button that closed the dialog to reveal the page underneath. Drives is
 * one kind of setting, not the place settings live.
 *
 * Now Drives is a tab like the others, and the modal is gone. Two things that
 * follow and are worth keeping:
 *
 *   - THE TAB IS IN THE URL (`/settings?tab=recovery`). Recovery is the one
 *     screen someone is sent to by documentation or by a support answer, and
 *     "click Settings, then Recovery" is a worse instruction than a link. It
 *     also means the browser back button steps through tabs, which is what a
 *     page is expected to do and what a dialog could not offer.
 *   - An unknown tab falls back to General rather than rendering nothing, so a
 *     stale link from an older build still lands somewhere useful.
 */
const TABS = [
  { id: 'general', label: 'General' },
  { id: 'drives', label: 'Drives' },
  { id: 'security', label: 'Security' },
  { id: 'recovery', label: 'Recovery' },
] as const;

type TabID = (typeof TABS)[number]['id'];

function isTab(v: string | null): v is TabID {
  return TABS.some((t) => t.id === v);
}

export function SettingsPage() {
  const { user } = useSession();
  const [params, setParams] = useSearchParams();

  const requested = params.get('tab');
  const active: TabID = isTab(requested) ? requested : 'general';

  // A junk or stale ?tab= is rewritten rather than left in the address bar
  // disagreeing with what is on screen. replace: true so it does not add a
  // history entry the user did not ask for.
  useEffect(() => {
    if (requested !== null && !isTab(requested)) {
      setParams({ tab: 'general' }, { replace: true });
    }
  }, [requested, setParams]);

  const [announced, setAnnounced] = useState('');
  useEffect(() => {
    const label = TABS.find((t) => t.id === active)?.label ?? '';
    setAnnounced(`${label} settings`);
  }, [active]);

  return (
    // NO WIDTH CONSTRAINT HERE. Layout already wraps every route in
    // `mx-auto max-w-content` (1280px), so a second `max-w-3xl` clamped
    // Settings to 768px while Files and Trash filled the space - the Drives
    // list in particular went from full width to half of it when it moved
    // under this route.
    //
    // Individual panels cap their own prose where a long line would be hard
    // to read; the page does not decide that for them.
    <div>
      <h1 className="text-title font-semibold text-text">Settings</h1>

      {/* Announced for screen readers, because switching tabs changes the
          whole panel with no other signal that anything happened. */}
      <span className="sr-only" role="status" aria-live="polite">
        {announced}
      </span>

      <div className="mt-4 flex gap-1 border-b border-line" role="tablist">
        {TABS.map((t) => (
          <button
            key={t.id}
            type="button"
            role="tab"
            id={`settings-tab-${t.id}`}
            aria-selected={active === t.id}
            aria-controls={`settings-panel-${t.id}`}
            onClick={() => setParams({ tab: t.id })}
            className={clsx(
              'border-b-2 px-3 py-2 text-label transition-colors duration-hover',
              active === t.id
                ? 'border-accent text-text'
                : 'border-transparent text-muted hover:text-text',
            )}
          >
            {t.label}
          </button>
        ))}
      </div>

      <div
        className="py-5"
        role="tabpanel"
        id={`settings-panel-${active}`}
        aria-labelledby={`settings-tab-${active}`}
      >
        {active === 'general' && <GeneralPanel email={user?.email ?? ''} />}
        {/* Drives renders its own heading and list; it is the same component
            the route used to render on its own, unchanged. */}
        {active === 'drives' && <Drives />}
        {active === 'security' && <SecurityPanel />}
        {active === 'recovery' && <RecoveryPanel />}
      </div>
    </div>
  );
}
