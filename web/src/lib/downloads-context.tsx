import {
  createContext,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
  useSyncExternalStore,
  type ReactNode,
} from 'react';

import { onSessionChange } from './api';
import { DownloadStore, type DownloadJob } from './downloads';
import {
  DesktopDownloadStore,
  desktopDownloadsAvailable,
  startDesktopDownload,
  type DesktopDownloadJob,
} from './desktop-downloads';

interface DownloadsValue {
  /** Browser-path jobs: "handed to your browser", no byte tracking (#15). */
  jobs: DownloadJob[];
  start: (name: string) => string;
  fail: (id: string, error: string) => void;
  dismiss: (id: string) => void;

  /**
   * True when the Go-side download path is available, decided by probing the
   * server. Fails closed: anything ambiguous is false, and false means the
   * ordinary a.click() path.
   */
  desktop: boolean;
  /** Go-side jobs, with real bytes. Empty in the browser. */
  desktopJobs: DesktopDownloadJob[];
  /** Starts a Go-side download. Throws if the server refuses. */
  startDesktop: (fileId: string) => Promise<void>;
  cancelDesktop: (id: string) => void;
  dismissDesktop: (id: string) => void;
  clearDesktopSettled: () => void;
}

const DownloadsContext = createContext<DownloadsValue | null>(null);

export function useDownloads(): DownloadsValue {
  const value = useContext(DownloadsContext);
  if (!value) throw new Error('useDownloads must be used inside DownloadsProvider');
  return value;
}

export function DownloadsProvider({ children }: { children: ReactNode }) {
  const storeRef = useRef<DownloadStore | null>(null);
  if (!storeRef.current) storeRef.current = new DownloadStore();
  const store = storeRef.current;

  const desktopRef = useRef<DesktopDownloadStore | null>(null);
  if (!desktopRef.current) desktopRef.current = new DesktopDownloadStore();
  const desktopStore = desktopRef.current;

  const jobs = useSyncExternalStore(store.subscribe, store.getSnapshot, store.getSnapshot);
  const desktopJobs = useSyncExternalStore(
    desktopStore.subscribe,
    desktopStore.getSnapshot,
    desktopStore.getSnapshot,
  );

  const [desktop, setDesktop] = useState(false);

  // Probe on mount AND whenever a session appears.
  //
  // Mount alone is not enough: this provider sits above the router so it
  // mounts before login, and /api/desktop/capabilities requires a session.
  // Measured in the running app — the probe fired at 16:47:24 and the login
  // landed at 16:47:33, so the only probe of the page's life was the one that
  // could not succeed, and every download went down the browser path.
  //
  // Until a probe answers, `desktop` is false: a slow or failed probe leaves
  // the browser path in place rather than a half-state.
  useEffect(() => {
    let live = true;

    const attempt = () => {
      void desktopDownloadsAvailable().then((available) => {
        if (!live || !available) return;
        setDesktop(true);
        // Re-attach to anything already running: a window reload must not
        // orphan a transfer the server is still performing.
        void desktopStore.resync();
      });
    };

    attempt();
    // A sign-in is the event that makes the pre-auth probe answerable.
    const off = onSessionChange((user) => {
      if (user) attempt();
    });

    return () => {
      live = false;
      off();
    };
  }, [desktopStore]);

  const value = useMemo<DownloadsValue>(
    () => ({
      jobs,
      start: (name) => store.start(name),
      fail: (id, error) => store.fail(id, error),
      dismiss: (id) => store.dismiss(id),

      desktop,
      desktopJobs,
      startDesktop: async (fileId) => {
        desktopStore.track(await startDesktopDownload(fileId));
      },
      cancelDesktop: (id) => void desktopStore.cancel(id),
      dismissDesktop: (id) => desktopStore.dismiss(id),
      clearDesktopSettled: () => desktopStore.dismissSettled(),
    }),
    [jobs, store, desktop, desktopJobs, desktopStore],
  );

  return <DownloadsContext.Provider value={value}>{children}</DownloadsContext.Provider>;
}
