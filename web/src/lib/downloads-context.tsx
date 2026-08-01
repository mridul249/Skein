import {
  createContext,
  useContext,
  useMemo,
  useRef,
  useSyncExternalStore,
  type ReactNode,
} from 'react';

import { DownloadStore, type DownloadJob } from './downloads';

interface DownloadsValue {
  jobs: DownloadJob[];
  start: (name: string) => string;
  fail: (id: string, error: string) => void;
  dismiss: (id: string) => void;
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

  const jobs = useSyncExternalStore(store.subscribe, store.getSnapshot, store.getSnapshot);

  const value = useMemo<DownloadsValue>(
    () => ({
      jobs,
      start: (name) => store.start(name),
      fail: (id, error) => store.fail(id, error),
      dismiss: (id) => store.dismiss(id),
    }),
    [jobs, store],
  );

  return <DownloadsContext.Provider value={value}>{children}</DownloadsContext.Provider>;
}
