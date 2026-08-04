/**
 * Which shell the frontend is running in.
 *
 * There is no build-time flag and no injected global: the desktop window
 * navigates to the same real http://127.0.0.1:<port> address the browser uses,
 * so the bundle is byte-identical in both (see cmd/skein-desktop/main.go). The
 * only thing that differs is the user agent, because the window is WebKitGTK
 * rather than Chrome/Firefox.
 *
 * This is used for COPY ONLY — telling the user where a download went. It must
 * never gate behaviour: a wrong answer should change a sentence, not break a
 * feature. A real capability signal for the Block 6 download path will be an
 * explicit runtime global, not a user-agent sniff.
 */
export function isDesktopShell(ua: string = navigator.userAgent): boolean {
  // Exclusion first, because Chrome on Linux also contains "AppleWebKit" and
  // is the obvious false positive. What is left that still reports WebKit on
  // Linux is the WebKitGTK window Wails embeds.
  if (/Chrome\/|Chromium\/|Firefox\/|Edg\//.test(ua)) return false;
  return /AppleWebKit\//.test(ua) && /Linux/.test(ua);
}

/**
 * Where a finished download lands, phrased for the shell in use.
 *
 * On desktop the webview writes to the user's Downloads folder and there is no
 * browser download manager to look in — saying "handed to your browser" there
 * is simply false, and the user can already see the file on disk.
 */
export function downloadDestination(desktop = isDesktopShell()): string {
  return desktop ? 'Saved to your Downloads folder' : 'Handed to your browser';
}
