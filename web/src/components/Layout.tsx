import { useQuery } from '@tanstack/react-query';
import { HardDrive, LogOut, ShieldCheck, Trash2, Files } from 'lucide-react';
import { NavLink, Outlet, useNavigate } from 'react-router-dom';
import clsx from 'clsx';

import { api } from '../lib/api';
import { QuotaRail } from './QuotaBar';
import { UploadList } from './UploadList';
import { DownloadList } from './DownloadList';
import { DesktopDownloadDrawer } from './DesktopDownloadDrawer';
import { useDownloads } from '../lib/downloads-context';
import { useSession } from '../lib/session';
import { Wordmark } from './Wordmark';

/**
 * The shell. Design.md §4: a fixed sidebar on `--surface` whose lower half is a
 * permanent per-account storage rail, and a single content column on `--canvas`.
 * Under 768px the sidebar collapses to a bottom bar.
 *
 * The rail is the clearest expression of pooling anywhere in the product — it
 * is the one place the five separate accounts are visible as five separate
 * accounts — so it is always on screen rather than a page someone must visit.
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
      'flex items-center gap-3 rounded-md px-3 py-2 text-body transition-colors duration-hover',
      // The current item is the one place in the sidebar the accent appears.
      isActive
        ? 'bg-raised font-semibold text-text'
        : 'text-muted hover:bg-raised hover:text-text',
    );

  return (
    <div className="flex min-h-screen flex-col md:flex-row">
      <aside
        className="flex shrink-0 flex-col border-border bg-surface
                   max-md:order-2 max-md:border-t md:h-screen md:w-sidebar md:border-r"
      >
        <div className="hidden items-center gap-2.5 px-5 pb-4 pt-5 md:flex">
          <img src="/mark.svg" width="20" height="20" alt="" aria-hidden />
          <Wordmark className="text-heading" />
        </div>

        <nav className="flex gap-1 px-3 max-md:justify-around max-md:py-2 md:flex-col">
          <NavLink to="/" end className={navClass}>
            {({ isActive }) => (
              <>
                <Files size={17} className={isActive ? 'text-accent' : ''} aria-hidden />
                <span className="max-md:sr-only">Files</span>
              </>
            )}
          </NavLink>
          <NavLink to="/trash" className={navClass}>
            {({ isActive }) => (
              <>
                <Trash2 size={17} className={isActive ? 'text-accent' : ''} aria-hidden />
                <span className="max-md:sr-only">Trash</span>
              </>
            )}
          </NavLink>
          <NavLink to="/settings" className={navClass}>
            {({ isActive }) => (
              <>
                <HardDrive size={17} className={isActive ? 'text-accent' : ''} aria-hidden />
                <span className="max-md:sr-only">Drives</span>
              </>
            )}
          </NavLink>
        </nav>

        <div className="hidden flex-1 md:block" />

        <div className="hidden md:block">
          <QuotaRail quota={quota} />
        </div>

        {/*
          The reassurance block. A tool that distributes your files across
          several accounts has to answer "is this safe" before it is asked, and
          it earns its space by answering with a mechanism — encrypted *before*
          it leaves the machine — rather than with an adjective.
        */}
        <div className="hidden px-3 pb-3 md:block">
          <div className="rounded-md border border-border bg-raised p-3">
            <div className="flex items-center gap-2">
              <ShieldCheck size={15} className="shrink-0 text-success" aria-hidden />
              <span className="text-label font-semibold text-text">Your data is safe</span>
            </div>
            <p className="mt-1 text-caption leading-relaxed text-muted">
              Encrypted on this machine before any shard leaves it.
            </p>
          </div>
        </div>

        <div className="hidden items-center justify-between gap-2 border-t border-border px-4 py-3 md:flex">
          <span className="truncate text-caption text-muted" title={user?.email}>
            {user?.email}
          </span>
          <button
            type="button"
            onClick={() => void signOut()}
            className="rounded p-1.5 text-muted transition-colors duration-hover hover:bg-raised hover:text-danger"
            aria-label="Sign out"
          >
            <LogOut size={15} aria-hidden />
          </button>
        </div>

        <button
          type="button"
          onClick={() => void signOut()}
          className="flex items-center justify-center gap-2 border-t border-border py-3
                     text-label text-muted md:hidden"
        >
          <LogOut size={15} aria-hidden />
          Sign out
        </button>
      </aside>

      <main className="min-w-0 flex-1 max-md:order-1 md:h-screen md:overflow-y-auto">
        <div className="mx-auto max-w-content px-6 py-6 md:px-8 md:py-8">
          {/*
            Above the Outlet, so an upload started on Files stays visible from
            Drives and Trash. #13: an upload the UI cannot see is an upload the
            user cannot cancel.
          */}
          <UploadList />
          <Downloads />
          <Outlet context={{ quota }} />
        </div>
      </main>
    </div>
  );
}

/**
 * One of two download UIs, never both.
 *
 * The desktop build streams through Go and can show real bytes; the browser
 * hands the transfer to the browser and can show nothing (#15). Rendering the
 * honest one for the shell in use is the whole point — a progress bar in the
 * browser would be the hoax loading this project keeps removing.
 */
function Downloads() {
  const { desktop, desktopJobs, cancelDesktop, dismissDesktop, clearDesktopSettled } =
    useDownloads();

  if (desktop) {
    return (
      <DesktopDownloadDrawer
        jobs={desktopJobs}
        onCancel={cancelDesktop}
        onDismiss={dismissDesktop}
        onClear={clearDesktopSettled}
      />
    );
  }
  return <DownloadList />;
}
