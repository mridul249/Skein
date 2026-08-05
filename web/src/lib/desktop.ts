/**
 * Desktop capability detection, by probing the server rather than sniffing the
 * user agent.
 *
 * The Go-side download route exists only in the desktop binary — it is behind
 * a build tag, and TestServerBinaryHasNoDesktopRoutes verifies the server
 * binary genuinely lacks it. So "is this route there?" is a direct question
 * about what this build can actually do, which is the thing we care about;
 * the user agent is a proxy for it at best.
 *
 * FAILS CLOSED. Unreachable, ambiguous, non-JSON, a timeout, or any error at
 * all means `false`, and false means the ordinary browser a.click() path,
 * unchanged. There is deliberately no third state: a half-enabled UI that
 * shows a progress drawer nothing feeds is worse than either endpoint.
 *
 * THE PROBE MUST BE AUTHENTICATED. /api/desktop/capabilities sits behind the
 * ordinary Auth middleware, so an unauthenticated probe gets a 401 — which
 * this function correctly reads as "not the desktop build", disabling the
 * feature in the very binary that has it. Defaulting to authedFetch rather
 * than fetch is what makes the answer mean what it says.
 */
import { authedFetch } from './api';

export interface DesktopCapabilities {
  desktopDownloads: boolean;
  downloadDir: string;
}

const BROWSER: DesktopCapabilities = { desktopDownloads: false, downloadDir: '' };

/** How long to wait before deciding this is not the desktop shell. */
const PROBE_TIMEOUT_MS = 2000;

let cached: Promise<DesktopCapabilities> | null = null;

/**
 * probeDesktop asks the server whether the Go-side download path is available.
 *
 * Cached for the lifetime of the page: the answer is a property of the binary,
 * which cannot change while it is running.
 */
export function probeDesktop(
  fetchImpl: typeof fetch = authedFetch,
  timeoutMs = PROBE_TIMEOUT_MS,
): Promise<DesktopCapabilities> {
  cached ??= runProbe(fetchImpl, timeoutMs);
  return cached;
}

/** Test seam: forget the cached answer. */
export function resetDesktopProbe(): void {
  cached = null;
}

async function runProbe(
  fetchImpl: typeof fetch,
  timeoutMs: number,
): Promise<DesktopCapabilities> {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), timeoutMs);

  try {
    const res = await fetchImpl('/api/desktop/capabilities', {
      signal: controller.signal,
      headers: { Accept: 'application/json' },
    });
    // 404 is the browser build's correct answer, not an error to report.
    if (!res.ok) return BROWSER;

    const body: unknown = await res.json();
    if (typeof body !== 'object' || body === null) return BROWSER;

    const parsed = body as { desktop_downloads?: unknown; download_dir?: unknown };
    // Only an explicit true enables it. Anything else — missing, truthy but
    // not boolean, a string — is ambiguous, and ambiguous means browser.
    if (parsed.desktop_downloads !== true) return BROWSER;

    return {
      desktopDownloads: true,
      downloadDir: typeof parsed.download_dir === 'string' ? parsed.download_dir : '',
    };
  } catch {
    // Network failure, abort, invalid JSON: all mean "not the desktop shell".
    return BROWSER;
  } finally {
    clearTimeout(timer);
  }
}
