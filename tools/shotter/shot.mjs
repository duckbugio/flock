// shot — capture a PNG screenshot of a web page using the system Chromium.
//
// This is the implementation behind the on-PATH `shot` wrapper baked into the
// runtime images (see adapters/*/Dockerfile). It drives the apt-installed system
// Chromium via puppeteer-core's `executablePath` — there is NO bundled/downloaded
// browser, which is why puppeteer-core (not full puppeteer) is used.
//
// CLI contract (committed):
//   shot <url> <out.png> [--full] [--width=N] [--height=N] [--wait=ms] [--scale=N]
//     --full        full-page capture, with a pre-capture autoscroll so lazy /
//                   scroll-reveal content has loaded before the shot is taken.
//     --width=N     viewport width  (default 1280)
//     --height=N    viewport height (default 800)
//     --wait=ms     extra settle time after load, in milliseconds (default 0,
//                   clamped to a 60000 ms max)
//     --scale=N     device scale factor / pixel density (default 1, clamped 1-3;
//                   higher = sharper but larger PNGs)
//
// Behaviour: on any failure (bad URL, navigation timeout, write error) it prints a
// clear message to stderr and exits non-zero. The PNG is written atomically — first
// to a temp file, then renamed into place — so a failed run never leaves a partial
// or zero-byte file behind.

import { rename, rm } from 'node:fs/promises';
import { dirname, join, resolve } from 'node:path';
import puppeteer from 'puppeteer-core';

function fail(msg) {
  console.error(`shot: ${msg}`);
  process.exit(1);
}

const [, , url, out, ...rest] = process.argv;
if (!url || !out) {
  console.error(
    'usage: shot <url> <out.png> [--full] [--width=N] [--height=N] [--wait=ms] [--scale=N]',
  );
  process.exit(1);
}

// Validate the URL before doing any work. We only ever drive Chromium at an
// http(s) page, so:
//   - a non-parseable URL fails fast with a clear message;
//   - only http:/https: schemes are allowed — this refuses file:, chrome:,
//     data:, etc., which would otherwise let a caller exfiltrate local files or
//     reach browser-internal pages;
//   - the cloud-metadata link-local range (169.254.0.0/16, incl. the well-known
//     169.254.169.254 endpoint) is refused to block SSRF against instance
//     metadata services.
// We deliberately DO NOT block loopback/localhost or RFC1918 private ranges: the
// dev team legitimately screenshots locally-running app servers (e.g.
// http://localhost:3000), so only non-http(s) schemes and the link-local range
// are rejected.
let parsedURL;
try {
  parsedURL = new URL(url);
} catch {
  fail(`invalid URL: ${url}`);
}
if (parsedURL.protocol !== 'http:' && parsedURL.protocol !== 'https:') {
  fail(`unsupported URL scheme "${parsedURL.protocol}": only http/https are allowed`);
}
{
  // Strip IPv6 brackets if present; link-local check only applies to IPv4 here.
  const host = parsedURL.hostname.replace(/^\[|\]$/g, '');
  if (/^169\.254\.\d{1,3}\.\d{1,3}$/.test(host)) {
    fail(`refusing link-local address ${host} (cloud-metadata SSRF guard)`);
  }
}

const opt = Object.fromEntries(
  rest.map((a) => a.replace(/^--/, '').split('=')),
);

// Parse a positive-integer option; fall back to `def` (with a stderr note) when
// the value is missing, non-finite, or <= 0.
function posIntOpt(name, value, def) {
  if (value === undefined) return def;
  const n = Number(value);
  if (!Number.isFinite(n) || n <= 0) {
    console.error(`shot: ignoring invalid --${name}=${value}; using ${def}`);
    return def;
  }
  return Math.trunc(n);
}

const width = posIntOpt('width', opt.width, 1280);
const height = posIntOpt('height', opt.height, 800);
const fullPage = 'full' in opt;

// Extra settle time: a non-negative integer, clamped to 60000 ms so an oversized
// value cannot overflow setTimeout (TimeoutOverflowWarning) or hang the run.
const WAIT_MAX = 60000;
let extraWait = posIntOpt('wait', opt.wait, 0);
if (extraWait > WAIT_MAX) {
  console.error(`shot: clamping --wait=${opt.wait} to ${WAIT_MAX}`);
  extraWait = WAIT_MAX;
}

// Device scale factor (pixel density): default 1 for smaller PNGs (less risk of
// hitting the outbox size cap); clamp to 1-3.
let scale = posIntOpt('scale', opt.scale, 1);
if (scale < 1) scale = 1;
if (scale > 3) {
  console.error(`shot: clamping --scale=${opt.scale} to 3`);
  scale = 3;
}

// Resolve the system Chromium; allow an explicit override for non-standard images.
const executablePath = process.env.CHROMIUM_PATH || '/usr/bin/chromium';

const outPath = resolve(out);
// Keep the temp file on the SAME filesystem as the destination so the final
// rename is atomic (a cross-device rename would fail with EXDEV).
const tmpPng = join(dirname(outPath), `.shot-${process.pid}.tmp.png`);

let browser;
try {
  browser = await puppeteer.launch({
    executablePath,
    headless: true,
    // Cap individual CDP calls so a hung/wedged page cannot block the protocol
    // connection indefinitely (default is 180s).
    protocolTimeout: 90_000,
    // Container-safe flags: no sandbox (we run unprivileged in a container) and
    // do not rely on /dev/shm, which is tiny by default and crashes Chromium.
    args: ['--no-sandbox', '--disable-dev-shm-usage'],
  });

  const page = await browser.newPage();
  await page.setViewport({ width, height, deviceScaleFactor: scale });

  // networkidle0-style wait: page is considered loaded once the network is quiet.
  await page.goto(url, { waitUntil: 'networkidle0', timeout: 60000 });

  // Trigger scroll-reveal / lazy content before a full-page capture. The page-side
  // promise is bounded so an infinite-scroll page cannot hang the run: it stops at
  // a wall-clock deadline, a max-iteration cap, when it reaches the bottom, OR when
  // scrollHeight stops growing across a couple of ticks (height has stabilised).
  if (fullPage) {
    await page.evaluate(
      () =>
        new Promise((resolve) => {
          const TICK_MS = 280;
          const DEADLINE_MS = 15000;
          const MAX_ITERS = Math.ceil(DEADLINE_MS / TICK_MS) + 5;
          const STABLE_TICKS = 2; // height unchanged this many ticks => stop
          const start = Date.now();
          let y = 0;
          let iters = 0;
          let lastHeight = -1;
          let stable = 0;
          const step = Math.round(window.innerHeight * 0.6);
          const docHeight = () =>
            document.body?.scrollHeight ??
            document.documentElement.scrollHeight;
          const timer = setInterval(() => {
            window.scrollTo(0, y);
            y += step;
            iters += 1;
            const h = docHeight();
            if (h === lastHeight) stable += 1;
            else {
              stable = 0;
              lastHeight = h;
            }
            const reachedBottom = y >= h + window.innerHeight;
            const timedOut = Date.now() - start >= DEADLINE_MS;
            const tooMany = iters >= MAX_ITERS;
            if (reachedBottom || timedOut || tooMany || stable >= STABLE_TICKS) {
              clearInterval(timer);
              window.scrollTo(0, 0);
              resolve();
            }
          }, TICK_MS);
        }),
    );
    await new Promise((r) => setTimeout(r, 500));
  }
  if (extraWait) await new Promise((r) => setTimeout(r, extraWait));

  // Render to a temp file first, then rename into place so a failure never leaves
  // a partial PNG at the destination.
  await page.screenshot({ path: tmpPng, fullPage });

  // Commit the rendered PNG BEFORE reading the (cosmetic) title, so a title()
  // throw after a successful capture can never delete a valid screenshot. The
  // title is best-effort metadata only.
  await rename(tmpPng, outPath);
  const title = await page.title().catch(() => '');
  console.log(JSON.stringify({ ok: true, url, out: outPath, title, fullPage }));
} catch (err) {
  // Drop any partial temp file before exiting non-zero.
  await rm(tmpPng, { force: true }).catch(() => {});
  fail(err && err.message ? err.message : String(err));
} finally {
  if (browser) await browser.close().catch(() => {});
}
