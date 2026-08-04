import assert from 'node:assert/strict';
import test from 'node:test';

import { downloadDestination, isDesktopShell } from '../src/lib/platform';

const WEBKITGTK =
  'Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/8.0 Safari/605.1.15';
const CHROME =
  'Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36';
const FIREFOX = 'Mozilla/5.0 (X11; Linux x86_64; rv:127.0) Gecko/20100101 Firefox/127.0';

test('the desktop webview is recognised', () => {
  assert.equal(isDesktopShell(WEBKITGTK), true);
});

// Chrome on Linux also contains "AppleWebKit", which is exactly the trap a
// naive check falls into.
test('desktop browsers are not mistaken for the desktop shell', () => {
  assert.equal(isDesktopShell(CHROME), false, 'Chrome contains AppleWebKit and must not match');
  assert.equal(isDesktopShell(FIREFOX), false);
});

test('the copy names the real destination for each shell', () => {
  assert.equal(downloadDestination(true), 'Saved to your Downloads folder');
  assert.equal(downloadDestination(false), 'Handed to your browser');
  // Never the browser phrasing on desktop: there is no browser to look in.
  assert.ok(!downloadDestination(true).includes('browser'));
});
