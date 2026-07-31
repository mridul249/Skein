import { useQuery } from '@tanstack/react-query';
import { HardDrive, LogOut, Settings, Trash2, Files } from 'lucide-react';
import { NavLink, Outlet, useNavigate } from 'react-router-dom';
import clsx from 'clsx';

import { api } from '../lib/api';
import { QuotaRail } from './QuotaBar';
import { UploadList } from './UploadList';
import { useSession } from '../lib/session';
import { Wordmark } from './Wordmark';

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
      'flex items-center gap-2 px-3 py-2 text-body transition-colors duration-hover',
      isActive ? 'bg-raised text-text' : 'text-muted hover:bg-raised hover:text-text',
    );

  return (
    <div className="flex min-h-screen flex-col md:flex-row">
      <aside
        className="flex shrink-0 flex-col border-border bg-surface
                   max-md:order-2 max-md:border-t md:h-screen md:w-sidebar md:border-r"
      >
        <div className="hidden items-center gap-2 px-4 py-3 md:flex">
          <img src="/mark.svg" width="18" height="18" alt="" aria-hidden />
          <Wordmark className="text-heading" />
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

        <div className="hidden items-center justify-between gap-2 border-t border-border px-4 py-2 md:flex">
          {/* Was --overlay0. Design.md §8 reserves that for placeholders and
              forbids it for anything a user must read; a signed-in identity
              is exactly that. */}
          <span className="truncate text-data-sm text-muted" title={user?.email}>
            {user?.email}
          </span>
          <button
            type="button"
            onClick={() => void signOut()}
            className="p-1 text-muted transition-colors duration-hover hover:text-danger"
            aria-label="Sign out"
          >
            <LogOut size={14} aria-hidden />
          </button>
        </div>

        <button
          type="button"
          onClick={() => void signOut()}
          className="flex items-center justify-center gap-2 border-t border-border py-2
                     text-label text-muted md:hidden"
        >
          <LogOut size={14} aria-hidden />
          Sign out
        </button>
      </aside>

      <main className="min-w-0 flex-1 max-md:order-1 md:h-screen md:overflow-y-auto">
        <div className="mx-auto max-w-content p-5 md:p-8">
          {/*
            Above the Outlet, so an upload started on Files stays visible from
            Drives and Trash. #13: an upload the UI cannot see is an upload the
            user cannot cancel.
          */}
          <UploadList />
          <Outlet context={{ quota }} />
        </div>
      </main>
    </div>
  );
}

/** Settings is a synonym for the Drives page; the icon links there. */
export const SettingsIcon = Settings;
