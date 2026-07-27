import { useQuery } from '@tanstack/react-query';
import { HardDrive, LogOut, Settings, Trash2, Files } from 'lucide-react';
import { NavLink, Outlet, useNavigate } from 'react-router-dom';
import clsx from 'clsx';

import { api } from '../lib/api';
import { QuotaRail } from './QuotaBar';
import { useSession } from '../lib/session';

/**
 * The shell. Design.md §4: a 240px fixed sidebar on mantle whose bottom
 * section is a permanent per-drive quota rail, and a single content column.
 * Under 768px the sidebar collapses to a bottom bar.
 */
export function Layout() {
  const navigate = useNavigate();
  const { user, setUser } = useSession();

  const { data: quota } = useQuery({
    queryKey: ['quota'],
    queryFn: api.quota,
    refetchInterval: 30_000,
  });

  async function signOut() {
    await api.logout();
    setUser(null);
    navigate('/login', { replace: true });
  }

  const navClass = ({ isActive }: { isActive: boolean }) =>
    clsx(
      'flex items-center gap-3 rounded px-3 py-2 text-body transition-colors duration-hover',
      isActive ? 'bg-surface0 text-text' : 'text-subtext0 hover:bg-surface0 hover:text-text',
    );

  return (
    <div className="flex min-h-screen flex-col md:flex-row">
      <aside
        className="flex shrink-0 flex-col border-surface0 bg-mantle
                   max-md:order-2 max-md:border-t md:h-screen md:w-sidebar md:border-r"
      >
        <div className="hidden px-4 py-5 md:block">
          <span className="font-display text-heading font-bold tracking-tight text-text">
            Skein
          </span>
        </div>

        <nav className="flex gap-1 px-3 max-md:justify-around max-md:py-2 md:flex-col md:py-0">
          <NavLink to="/" end className={navClass}>
            <Files size={16} aria-hidden />
            <span className="max-md:sr-only">Files</span>
          </NavLink>
          <NavLink to="/trash" className={navClass}>
            <Trash2 size={16} aria-hidden />
            <span className="max-md:sr-only">Trash</span>
          </NavLink>
          <NavLink to="/settings" className={navClass}>
            <HardDrive size={16} aria-hidden />
            <span className="max-md:sr-only">Drives</span>
          </NavLink>
        </nav>

        <div className="hidden flex-1 md:block" />

        <div className="hidden md:block">
          <QuotaRail quota={quota} />
        </div>

        <div className="hidden items-center justify-between gap-2 border-t border-surface0 px-4 py-3 md:flex">
          <span className="truncate text-data-sm text-overlay0" title={user?.email}>
            {user?.email}
          </span>
          <button
            type="button"
            onClick={() => void signOut()}
            className="rounded p-1 text-subtext0 transition-colors duration-hover hover:text-red"
            aria-label="Sign out"
          >
            <LogOut size={14} aria-hidden />
          </button>
        </div>

        <button
          type="button"
          onClick={() => void signOut()}
          className="flex items-center justify-center gap-2 border-t border-surface0 py-2
                     text-label text-subtext0 md:hidden"
        >
          <LogOut size={14} aria-hidden />
          Sign out
        </button>
      </aside>

      <main className="min-w-0 flex-1 max-md:order-1 md:h-screen md:overflow-y-auto">
        <div className="mx-auto max-w-content p-5 md:p-8">
          <Outlet context={{ quota }} />
        </div>
      </main>
    </div>
  );
}

/** Settings is a synonym for the Drives page; the icon links there. */
export const SettingsIcon = Settings;
