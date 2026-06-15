// shot — capture a PNG screenshot of a web page using the system Chromium.
//
// This is the implementation behind the on-PATH `shot` wrapper baked into the
// runtime images (see adapters/*/Dockerfile). It drives the apt-installed system
// Chromium via puppeteer-core's `executablePath` — there is NO bundled/downloaded
// browser, which is why puppeteer-core (not full puppeteer) is used.
//
// CLI contract (committed):
//   shot <url> <out.png> [--full] [--width=N] [--height=N] [--wait=ms]
//     --full        full-page capture, with a pre-capture autoscroll so lazy /
//                   scroll-reveal content has loaded before the shot is taken.
//     --width=N     viewport width  (default 1280)
//     --height=N    viewport height (default 800)
//     --wait=ms     extra settle time after load, in milliseconds (default 0)
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
    'usage: shot <url> <out.png> [--full] [--width=N] [--height=N] [--wait=ms]',
  );
  process.exit(1);
}

const opt = Object.fromEntries(
  rest.map((a) => a.replace(/^--/, '').split('=')),
);
const width = Number(opt.width) || 1280;
const height = Number(opt.height) || 800;
const fullPage = 'full' in opt;
const extraWait = Number(opt.wait) || 0;

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
    // Container-safe flags: no sandbox (we run unprivileged in a container) and
    // do not rely on /dev/shm, which is tiny by default and crashes Chromium.
    args: ['--no-sandbox', '--disable-dev-shm-usage'],
  });

  const page = await browser.newPage();
  await page.setViewport({ width, height, deviceScaleFactor: 2 });

  // networkidle0-style wait: page is considered loaded once the network is quiet.
  await page.goto(url, { waitUntil: 'networkidle0', timeout: 60000 });

  // Trigger scroll-reveal / lazy content before a full-page capture.
  if (fullPage) {
    await page.evaluate(
      () =>
        new Promise((resolve) => {
          let y = 0;
          const step = Math.round(window.innerHeight * 0.6);
          const timer = setInterval(() => {
            window.scrollTo(0, y);
            y += step;
            const docHeight =
              document.body?.scrollHeight ??
              document.documentElement.scrollHeight;
            if (y >= docHeight + window.innerHeight) {
              clearInterval(timer);
              window.scrollTo(0, 0);
              resolve();
            }
          }, 280);
        }),
    );
    await new Promise((r) => setTimeout(r, 500));
  }
  if (extraWait) await new Promise((r) => setTimeout(r, extraWait));

  // Render to a temp file first, then rename into place so a failure never leaves
  // a partial PNG at the destination.
  await page.screenshot({ path: tmpPng, fullPage });

  const title = await page.title();
  await rename(tmpPng, outPath);
  console.log(JSON.stringify({ ok: true, url, out: outPath, title, fullPage }));
} catch (err) {
  // Drop any partial temp file before exiting non-zero.
  await rm(tmpPng, { force: true }).catch(() => {});
  fail(err && err.message ? err.message : String(err));
} finally {
  if (browser) await browser.close().catch(() => {});
}
