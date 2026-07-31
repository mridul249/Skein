import {
  createContext,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useSyncExternalStore,
  type ReactNode,
} from 'react';
import { useQueryClient } from '@tanstack/react-query';

import { api } from './api';
import {
  UploadStore,
  installUnloadGuard,
  isActive,
  type UploadJob,
  type Uploader,
} from './uploads';

/**
 * Binds the upload store to React.
 *
 * The provider is mounted above BrowserRouter, so it is not unmounted by
 * navigation — that placement is the entire fix for known issue #13, and
 * moving it inside a route would silently restore the bug.
 */
interface UploadsValue {
  jobs: UploadJob[];
  active: number;
  start: (file: File, folderId: string | null) => void;
  cancel: (id: string) => void;
  dismiss: (id: string) => void;
}

const UploadsContext = createContext<UploadsValue | null>(null);

export function useUploads(): UploadsValue {
  const value = useContext(UploadsContext);
  if (!value) throw new Error('useUploads must be used inside UploadsProvider');
  return value;
}

export function UploadsProvider({
  children,
  uploader,
}: {
  children: ReactNode;
  /**
   * Overrides how bytes actually get sent. Production leaves this unset and
   * gets `api.upload`.
   *
   * `UploadStore` already takes an injected uploader; this only exposes that
   * seam one level up so the browser fixture can drive real `sending` and
   * `finishing` states without a network. Without it, the only reachable
   * state in a fixture is `error`, and the honest-progress rule — a bar must
   * never claim completion the server has not reported — would be untestable.
   */
  uploader?: Uploader;
}) {
  const qc = useQueryClient();

  // The store is created once for the life of the app. A store rebuilt on
  // render would lose every in-flight job, which is the bug in another shape.
  const storeRef = useRef<UploadStore | null>(null);
  if (!storeRef.current) {
    storeRef.current = new UploadStore(
      uploader ??
        ((file, folderId, onProgress, signal) =>
        api.upload(
          file as File,
          folderId,
          (sent, total) => onProgress(sent, total),
          signal,
        )),
      () => {
        // A settled upload changes the listing and the quota, wherever the
        // user happens to be standing.
        void qc.invalidateQueries({ queryKey: ['files'] });
        void qc.invalidateQueries({ queryKey: ['quota'] });
      },
    );
  }
  const store = storeRef.current;

  const jobs = useSyncExternalStore(store.subscribe, store.getSnapshot, store.getSnapshot);

  // Attached only while something is in flight, and never for downloads,
  // which the browser owns and which survive the tab closing.
  useEffect(() => installUnloadGuard(store, window), [store]);

  const value = useMemo<UploadsValue>(
    () => ({
      jobs,
      active: jobs.filter(isActive).length,
      start: (file, folderId) => store.start(file, folderId),
      cancel: (id) => store.cancel(id),
      dismiss: (id) => store.dismiss(id),
    }),
    [jobs, store],
  );

  return <UploadsContext.Provider value={value}>{children}</UploadsContext.Provider>;
}
