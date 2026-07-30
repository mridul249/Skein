/**
 * Run with: npm run test  (node --test --experimental-strip-types)
 *
 * No test framework is installed and none is added here: Rules.md §3 requires
 * asking before a new dependency, and Node has run TypeScript tests natively
 * since 22.6. That constraint is why UploadStore is a plain module — it is
 * testable without a DOM, which is also the property that lets it outlive the
 * route.
 */
import assert from 'node:assert/strict';
import test from 'node:test';

import {
  UploadStore,
  installUnloadGuard,
  isActive,
  type UnloadTarget,
  type UploadFile,
  type UploadJob,
} from '../src/lib/uploads';

const file = (name: string, size: number): UploadFile => ({ name, size });

/**
 * A controllable uploader. Nothing here touches the network, so a test can
 * hold an upload mid-flight for as long as it likes — which is exactly the
 * state the incident happened in.
 */
function manualUploader() {
  const inFlight: {
    name: string;
    progress: (sent: number, total: number) => void;
    resolve: () => void;
    reject: (err: Error) => void;
    signal: AbortSignal;
  }[] = [];

  const uploader = (
    f: UploadFile,
    _folderId: string | null,
    onProgress: (sent: number, total: number) => void,
    signal: AbortSignal,
  ) =>
    new Promise<unknown>((resolve, reject) => {
      const entry = {
        name: f.name,
        progress: onProgress,
        resolve: () => resolve(undefined),
        reject,
        signal,
      };
      inFlight.push(entry);
      // Mirror XHR: aborting the signal rejects the promise.
      signal.addEventListener('abort', () => reject(new Error('Upload cancelled.')));
    });

  return { uploader, inFlight };
}

/** A component that mounts, renders, and unmounts — i.e. a route. */
function mountRoute(store: UploadStore) {
  let seen: UploadJob[] = store.getSnapshot();
  const unsubscribe = store.subscribe(() => {
    seen = store.getSnapshot();
  });
  return {
    jobs: () => seen,
    unmount: () => unsubscribe(),
  };
}

const settle = () => new Promise<void>((r) => setImmediate(() => r()));

/** Indexing with noUncheckedIndexedAccess on: a missing element is a bug in the test. */
function nth<T>(items: readonly T[], i: number): T {
  const value = items[i];
  if (value === undefined) throw new Error(`no element ${i}`);
  return value;
}

test('an upload survives navigating away and back', async () => {
  const { uploader, inFlight } = manualUploader();
  const store = new UploadStore(uploader);

  const filesPage = mountRoute(store);
  store.start(file('big.bin', 1000), null);
  nth(inFlight, 0).progress(400, 1000);

  assert.equal(filesPage.jobs().length, 1);
  assert.equal(nth(filesPage.jobs(), 0).sent, 400);

  // Navigate to Drives: the Files route unmounts.
  filesPage.unmount();

  // The upload keeps going while no route is watching. Nothing aborts it.
  nth(inFlight, 0).progress(700, 1000);
  assert.equal(nth(inFlight, 0).signal.aborted, false, 'unmount must not abort a healthy upload');

  // Navigate back: a fresh mount sees the same job, further along.
  const backAgain = mountRoute(store);
  assert.equal(backAgain.jobs().length, 1);
  assert.equal(nth(backAgain.jobs(), 0).sent, 700);
  assert.equal(nth(backAgain.jobs(), 0).status, 'sending');
  assert.equal(nth(backAgain.jobs(), 0).name, 'big.bin');
});

test('two concurrent uploads both survive a navigation', async () => {
  const { uploader, inFlight } = manualUploader();
  const store = new UploadStore(uploader);

  const page = mountRoute(store);
  const first = store.start(file('one.bin', 100), null);
  const second = store.start(file('two.bin', 200), null);

  nth(inFlight, 0).progress(50, 100);
  nth(inFlight, 1).progress(20, 200);
  page.unmount();

  // Both continue, independently, with nobody mounted.
  nth(inFlight, 0).progress(90, 100);
  nth(inFlight, 1).progress(150, 200);

  const back = mountRoute(store);
  const jobs = back.jobs();
  assert.equal(jobs.length, 2, 'both jobs must survive');

  const one = jobs.find((j) => j.id === first);
  const two = jobs.find((j) => j.id === second);
  assert.equal(one?.sent, 90);
  assert.equal(two?.sent, 150);

  // Finishing one must not disturb the other.
  nth(inFlight, 0).resolve();
  await settle();
  assert.equal(back.jobs().find((j) => j.id === first)?.status, 'done');
  assert.equal(back.jobs().find((j) => j.id === second)?.status, 'sending');
  assert.equal(back.jobs().find((j) => j.id === second)?.sent, 150);
  assert.equal(nth(inFlight, 1).signal.aborted, false);
});

test('cancel works from a route that did not start the upload', async () => {
  const { uploader, inFlight } = manualUploader();
  const store = new UploadStore(uploader);

  const filesPage = mountRoute(store);
  const id = store.start(file('big.bin', 1000), null);
  nth(inFlight, 0).progress(300, 1000);
  filesPage.unmount();

  // Cancelling from elsewhere entirely.
  const drivesPage = mountRoute(store);
  store.cancel(id);
  await settle();

  assert.equal(nth(inFlight, 0).signal.aborted, true, 'the AbortController must still be reachable');
  assert.equal(nth(drivesPage.jobs(), 0).status, 'cancelled');
  assert.equal(nth(drivesPage.jobs(), 0).error, undefined, 'a cancel is not an error');
});

test('a dead request becomes an explicit failure, not a frozen bar', async () => {
  const { uploader, inFlight } = manualUploader();
  const store = new UploadStore(uploader);
  const page = mountRoute(store);

  store.start(file('big.bin', 1000), null);
  nth(inFlight, 0).progress(400, 1000);
  assert.equal(nth(page.jobs(), 0).status, 'sending');

  // The connection drops, as it does when the server goes away mid-upload.
  nth(inFlight, 0).reject(new Error('The connection dropped.'));
  await settle();

  const job = nth(page.jobs(), 0);
  assert.equal(job.status, 'error', 'a dead upload must not sit in `sending` forever');
  assert.equal(job.error, 'The connection dropped.');
  assert.equal(isActive(job), false);
  // The job is still listed. Silently vanishing is the other half of the bug.
  assert.equal(page.jobs().length, 1);
});

test('progress reaching 100% is `finishing`, not `done`', async () => {
  const { uploader, inFlight } = manualUploader();
  const store = new UploadStore(uploader);
  const page = mountRoute(store);

  store.start(file('big.bin', 1000), null);

  // The browser has flushed every byte to the socket. The server has not yet
  // said it wrote them to a drive.
  nth(inFlight, 0).progress(1000, 1000);
  assert.equal(nth(page.jobs(), 0).status, 'finishing');
  assert.equal(isActive(nth(page.jobs(), 0)), true, 'still in flight until the server answers');

  nth(inFlight, 0).resolve();
  await settle();
  assert.equal(nth(page.jobs(), 0).status, 'done');
});

test('a late progress event cannot resurrect a cancelled job', async () => {
  const { uploader, inFlight } = manualUploader();
  const store = new UploadStore(uploader);
  const page = mountRoute(store);

  const id = store.start(file('big.bin', 1000), null);
  store.cancel(id);
  await settle();
  assert.equal(nth(page.jobs(), 0).status, 'cancelled');

  // An event still queued in the network stack when the abort landed.
  nth(inFlight, 0).progress(900, 1000);
  assert.equal(nth(page.jobs(), 0).status, 'cancelled');
  assert.equal(nth(page.jobs(), 0).sent, 0);
});

test('dismiss removes a settled job but refuses a live one', async () => {
  const { uploader, inFlight } = manualUploader();
  const store = new UploadStore(uploader);
  const page = mountRoute(store);

  const id = store.start(file('big.bin', 1000), null);
  store.dismiss(id);
  assert.equal(page.jobs().length, 1, 'dismissing a live upload would orphan it again');

  nth(inFlight, 0).resolve();
  await settle();
  store.dismiss(id);
  assert.equal(page.jobs().length, 0);
});

/** A window stand-in that records what is attached. */
function fakeWindow(): UnloadTarget & { count: () => number } {
  const handlers = new Set<(e: BeforeUnloadEvent) => void>();
  return {
    addEventListener: (_t, fn) => {
      handlers.add(fn);
    },
    removeEventListener: (_t, fn) => {
      handlers.delete(fn);
    },
    count: () => handlers.size,
  };
}

test('beforeunload warns only while an upload is in flight', async () => {
  const { uploader, inFlight } = manualUploader();
  const store = new UploadStore(uploader);
  const win = fakeWindow();
  const uninstall = installUnloadGuard(store, win);

  assert.equal(win.count(), 0, 'a quiet app must not prompt');

  const id = store.start(file('big.bin', 1000), null);
  assert.equal(win.count(), 1, 'an in-flight upload must prompt');

  // `finishing` must still prompt. Every byte has been handed to the socket,
  // but the server is still writing shards to Drive — the window where a
  // reload is most expensive and least obviously dangerous.
  nth(inFlight, 0).progress(1000, 1000);
  assert.equal(nth(store.getSnapshot(), 0).status, 'finishing');
  assert.equal(win.count(), 1, 'finishing is still in flight and must still prompt');

  store.cancel(id);
  await settle();
  assert.equal(win.count(), 0, 'nothing in flight, no prompt');

  // And again for a completed upload rather than a cancelled one.
  store.start(file('second.bin', 10), null);
  assert.equal(win.count(), 1);
  nth(inFlight, 1).resolve();
  await settle();
  assert.equal(win.count(), 0);

  uninstall();
  assert.equal(win.count(), 0);
});

test('beforeunload does not fire for downloads', async () => {
  // Downloads are browser-managed since the capability-URL change: the
  // transfer is owned by the browser process and survives the tab closing, so
  // a warning would be false. This is true by construction — the store has no
  // download concept at all — and this test exists to keep it that way if
  // someone later routes downloads through here.
  const { uploader } = manualUploader();
  const store = new UploadStore(uploader);
  const win = fakeWindow();
  installUnloadGuard(store, win);

  const surface = Object.getOwnPropertyNames(
    Object.getPrototypeOf(store) as object,
  ).concat(Object.keys(store));
  for (const name of surface) {
    assert.ok(
      !/download/i.test(name),
      `UploadStore gained a download-shaped member (${name}); downloads must not register here`,
    );
  }

  // A download in progress is not modelled, so nothing can make this prompt.
  assert.equal(store.activeCount(), 0);
  assert.equal(win.count(), 0);
});

test('onSettled fires once per job, with its terminal state', async () => {
  const { uploader, inFlight } = manualUploader();
  const settled: UploadJob[] = [];
  const store = new UploadStore(uploader, (job) => settled.push(job));

  store.start(file('a.bin', 10), null);
  store.start(file('b.bin', 10), null);
  nth(inFlight, 0).resolve();
  nth(inFlight, 1).reject(new Error('Needs 15.0 GB. 13.0 GB free across 2 drives.'));
  await settle();

  assert.equal(settled.length, 2);
  assert.deepEqual(
    settled.map((j) => [j.name, j.status]).sort(),
    [
      ['a.bin', 'done'],
      ['b.bin', 'error'],
    ],
  );
});
