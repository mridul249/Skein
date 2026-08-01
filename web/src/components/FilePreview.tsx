import { useEffect, useState } from 'react';
import { FileWarning } from 'lucide-react';

import { ApiError, api, type FileItem } from '../lib/api';
import { bytes } from '../lib/format';

/**
 * Inline preview for a selected file. Known issue #16.
 *
 * **This is the first thing in the product that issues a Range request.** The
 * server's ranged read has been correct and tested since Gate 0 — including
 * the shard-boundary case, where a range spans two shards on two different
 * drives and the reader has to map plaintext offsets through the encryption
 * frame layout — but nothing in the UI could reach it, because no element that
 * could issue one was ever rendered. A `<video>` seeking is that element.
 *
 * **Security: the guess below is a rendering hint, never an authorisation.**
 * `previewKind` reads the filename extension, which is client-supplied and
 * therefore a claim. It only decides *which element to try*. What actually
 * gets served is decided server-side by sniffing the real bytes against the
 * inline allowlist (Rules.md §2.3): anything not on that list comes back as
 * `application/octet-stream` with `Content-Disposition: attachment`, and the
 * element simply fails to load. A file named `.png` that contains HTML is
 * downloaded, not executed — the guess cannot promote it.
 *
 * The URL is a capability grant: scoped to one file and one user, valid ~15
 * minutes. It is minted per selection and never persisted.
 */
type PreviewKind = 'image' | 'video' | 'audio' | null;

/**
 * Above this, an image is not previewed inline.
 *
 * **The limit is decode cost, not transfer cost.** Transfer is already free to
 * us: the browser streams the bytes itself and manages its own memory, and a
 * striped file costs no more than an unstriped one. What is not free is
 * decoding — a JPEG decompresses to `width x height x 4` bytes in the
 * compositor regardless of how small the file is. A 24-megapixel photo is
 * perhaps 8 MB on disk and about **96 MB decoded**, and several of those held
 * at once is how a tab gets killed.
 *
 * File size is a poor proxy for pixel count, but it is the only number
 * available before fetching anything, and it is wrong in the safe direction:
 * a highly compressed image is small and decodes large, so the threshold has
 * to be conservative. 24 MB is roughly where a photograph stops being a
 * photograph and starts being a scan or a RAW export — the point at which
 * someone wants the file rather than a look at it.
 *
 * Video and audio are exempt: they decode a frame at a time and the element
 * only pulls what it plays, which is the entire reason `preload="metadata"` is
 * set below.
 */
const MAX_INLINE_IMAGE_BYTES = 24 * 1024 * 1024;

const IMAGE = ['png', 'jpg', 'jpeg', 'gif', 'webp', 'avif'];
const VIDEO = ['mp4', 'webm'];
const AUDIO = ['mp3', 'ogg', 'oga', 'wav'];

export function previewKind(name: string): PreviewKind {
  const ext = name.split('.').pop()?.toLowerCase() ?? '';
  if (IMAGE.includes(ext)) return 'image';
  if (VIDEO.includes(ext)) return 'video';
  if (AUDIO.includes(ext)) return 'audio';
  return null;
}

export function FilePreview({ file }: { file: FileItem }) {
  const kind = previewKind(file.name);
  const tooBigToDecode = kind === 'image' && file.size_bytes > MAX_INLINE_IMAGE_BYTES;
  const [fullSizeURL, setFullSizeURL] = useState<string | null>(null);

  // Minted only when asked for, so an oversized image costs nothing until
  // someone decides they want it. A grant is a credential with a short life;
  // there is no reason to hand one out for a preview nobody opened.
  const openFullSize = async () => {
    try {
      const u = await api.contentURL(file.id);
      setFullSizeURL(u);
      window.open(u, '_blank', 'noopener,noreferrer');
    } catch (err: unknown) {
      setError(err instanceof ApiError ? err.message : 'Could not open that file.');
    }
  };
  const [url, setUrl] = useState<string | null>(null);
  const [error, setError] = useState('');

  useEffect(() => {
    if (!kind || tooBigToDecode) return;
    let live = true;
    setUrl(null);
    setError('');

    api
      .contentURL(file.id)
      .then((u) => {
        // The selection can change while the mint is in flight; a late
        // response must not point the element at the previous file.
        if (live) setUrl(u);
      })
      .catch((err: unknown) => {
        if (!live) return;
        setError(err instanceof ApiError ? err.message : 'Could not load a preview.');
      });

    return () => {
      live = false;
    };
  }, [file.id, kind, tooBigToDecode]);

  if (!kind) return null;

  if (tooBigToDecode) {
    return (
      <section>
        <h3 className="mb-2 text-label font-semibold text-muted">Preview</h3>
        <div className="rounded border border-line bg-canvas p-4">
          <p className="text-caption text-muted">
            Too large to show here - a {bytes(file.size_bytes)} image decodes to far more
            than that in memory.
          </p>
          <a
            href={fullSizeURL ?? undefined}
            target="_blank" 
            rel="noopener noreferrer"
            onClick={(e) => {
              if (!fullSizeURL) {
                e.preventDefault();
                void openFullSize();
              }
            }}
            className="mt-2 inline-block text-caption text-accent underline underline-offset-2"
          >
            View full size
          </a>
        </div>
      </section>
    );
}

  return (
    <section>
      <h3 className="mb-2 text-label font-semibold text-muted">Preview</h3>

      <div className="flex min-h-[8rem] items-center justify-center overflow-hidden rounded border border-line bg-canvas">
        {error && (
          <p className="flex items-center gap-2 p-4 text-caption text-muted">
            <FileWarning size={14} className="shrink-0 text-warning" aria-hidden />
            {error}
          </p>
        )}

        {!error && !url && (
          <p className="p-4 text-caption text-muted">Loading preview…</p>
        )}

        {url && !error && kind === 'image' && (
          <img
            src={url}
            alt={file.name}
            className="max-h-64 w-auto max-w-full object-contain"
            onError={() => setError('This file cannot be shown here. Download it instead.')}
          />
        )}

        {url && !error && kind === 'video' && (
          // `preload="metadata"` on purpose: enough for dimensions and a
          // duration, without pulling the whole file across every drive it is
          // striped over. Seeking is what exercises the ranged read.
          // Never autoplay — this is a file manager, not a feed.
          <video
            src={url}
            controls
            preload="metadata"
            className="max-h-64 w-full"
            onError={() => setError('This file cannot be played here. Download it instead.')}
          />
        )}

        {url && !error && kind === 'audio' && (
          <audio
            src={url}
            controls
            preload="metadata"
            className="w-full p-4"
            onError={() => setError('This file cannot be played here. Download it instead.')}
          />
        )}
      </div>

      {kind !== 'image' && !error && (
        <p className="mt-1.5 text-caption text-faint">
          Streamed from its shards on demand. Nothing is written to this device.
        </p>
      )}
    </section>
  );
}
