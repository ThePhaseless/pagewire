// Browser end-to-end harness: builds the real binary, runs it in sim mode and drives headless
// Chromium through playwright-core. Test infrastructure, not application code.
import { existsSync, mkdirSync, mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { type Browser, type BrowserContextOptions, chromium, type Page } from "playwright-core";

// The e2e run must see no fake DOM: package.json's e2e script sets PRINTSCAN_E2E=1.
if (typeof window !== "undefined") throw new Error("happy-dom is registered in the e2e run");

export const repoRoot = resolve(import.meta.dir, "../..");
export const artifactsDir = join(import.meta.dir, "artifacts");
const binary = join(repoRoot, ".cache/printscan");

/** Console errors matching one of these are ignored. Kept empty: fix the cause instead. */
export const consoleAllowlist: RegExp[] = [];

// Chromium logs this for every 4xx/5xx fetch; API statuses are checked by the API status rule.
const LOAD_FAILED = "Failed to load resource: the server responded with a status of";

let built: Promise<void> | undefined;

/** Builds the binary once per run; the embedded UI comes from ui/dist (`bun run build`). */
export function buildBinary(): Promise<void> {
  built ??= (async () => {
    if (!existsSync(join(repoRoot, "ui/dist/index.html")))
      throw new Error("ui/dist/index.html is missing: run `bun run build` before `bun run e2e`");
    const p = Bun.spawn(["go", "build", "-o", binary, "./cmd/printscan"], {
      cwd: repoRoot,
      stdout: "inherit",
      stderr: "inherit",
    });
    if ((await p.exited) !== 0) throw new Error(`go build failed with exit code ${p.exitCode}`);
  })();
  return built;
}

function freePort(): number {
  const s = Bun.serve({ hostname: "127.0.0.1", port: 0, fetch: () => new Response() });
  const port = s.port;
  s.stop(true);
  if (port === undefined) throw new Error("no free port");
  return port;
}

export type Server = {
  url: string;
  dataDir: string;
  configPath: string;
  stop: () => Promise<void>;
};

/** Starts `serve --backend=sim` on a fresh data dir and waits for /healthz; `args` are extra flags. */
export async function startServer(
  name: string,
  simDevice = "ippusb-mfp",
  args: string[] = [],
): Promise<Server> {
  await buildBinary();
  mkdirSync(artifactsDir, { recursive: true });
  const dataDir = mkdtempSync(join(tmpdir(), "printscan-e2e-"));
  const configPath = join(dataDir, "printscan.yaml");
  const url = `http://127.0.0.1:${freePort()}`;
  const log = join(artifactsDir, `server-${name}.log`);
  // A file, not a pipe: an unread pipe would fill up and stall the server.
  const proc = Bun.spawn(
    [
      binary,
      "serve",
      "--backend=sim",
      `--sim-device=${simDevice}`,
      `--listen=${new URL(url).host}`,
      "--internal-listen=127.0.0.1:0",
      `--data-dir=${dataDir}`,
      `--config=${configPath}`,
      "--log-level=debug",
      ...args,
    ],
    { env: { ...process.env, TZ: "Europe/Warsaw" }, stdout: Bun.file(log), stderr: Bun.file(log) },
  );

  const stop = async () => {
    proc.kill("SIGTERM");
    const exited = await Promise.race([proc.exited.then(() => true), Bun.sleep(5000)]);
    if (!exited) proc.kill("SIGKILL");
    await proc.exited;
    rmSync(dataDir, { recursive: true, force: true });
  };

  const deadline = Date.now() + 20_000;
  let outcome = "timed out after 20 s";
  for (;;) {
    if (proc.exitCode !== null) {
      outcome = `the process exited with code ${proc.exitCode}`;
      break;
    }
    try {
      if ((await fetch(`${url}/healthz`)).ok) {
        console.log(`server ${name} (${simDevice}): /healthz ok at ${url}`);
        return { url, dataDir, configPath, stop };
      }
    } catch {
      // Not listening yet.
    }
    if (Date.now() > deadline) break;
    await Bun.sleep(500);
  }
  await stop();
  const tail = (await Bun.file(log).text()).split("\n").slice(-30).join("\n");
  throw new Error(`server ${name}: /healthz ${outcome}. Log tail (${log}):\n${tail}`);
}

let browser: Promise<Browser> | undefined;

export function getBrowser(): Promise<Browser> {
  browser ??= chromium.launch({ headless: true });
  return browser;
}

export async function closeBrowser() {
  if (browser) await (await browser).close();
  browser = undefined;
}

export type Viewport = { name: string; width: number; height: number };

type ApiExpectation = { method: string; path: string | RegExp; status: number };

/** One scenario's page plus the console and API status rules. */
export class Session {
  readonly consoleErrors: string[] = [];
  readonly apiErrors: { method: string; path: string; status: number }[] = [];
  private readonly expected: ApiExpectation[] = [];

  constructor(
    readonly page: Page,
    readonly server: Server,
    readonly viewport: Viewport,
  ) {
    page.on("console", (m) => {
      if (m.type() !== "error") return;
      const text = m.text();
      const loc = m.location();
      if (text.startsWith(LOAD_FAILED) && pathOf(loc.url).startsWith("/api/v1/")) return;
      if (consoleAllowlist.some((re) => re.test(text))) return;
      this.consoleErrors.push(`console.error: ${text} (at ${loc.url}:${loc.lineNumber})`);
    });
    page.on("pageerror", (e) => this.consoleErrors.push(`pageerror: ${e.stack ?? e.message}`));
    page.on("response", (r) => {
      const path = pathOf(r.url());
      if (r.status() >= 400 && path.startsWith("/api/v1/"))
        this.apiErrors.push({ method: r.request().method(), path, status: r.status() });
    });
  }

  /** Declares an API error status this scenario's flow produces by design. */
  expectApiStatus(method: string, path: string | RegExp, status: number) {
    this.expected.push({ method, path, status });
  }

  /** Every console error, page error and undeclared API error status recorded so far. */
  failures(): string[] {
    const unexpected = this.apiErrors.filter(
      (a) =>
        !this.expected.some(
          (e) =>
            e.method === a.method &&
            e.status === a.status &&
            (typeof e.path === "string" ? e.path === a.path : e.path.test(a.path)),
        ),
    );
    return [
      ...this.consoleErrors,
      ...unexpected.map((a) => `unexpected API status: ${a.method} ${a.path} ${a.status}`),
    ];
  }

  /** Forgets what was recorded (the console-rule self-test uses it after asserting). */
  clear() {
    this.consoleErrors.length = 0;
    this.apiErrors.length = 0;
  }

  goto(path: string) {
    return this.page.goto(this.server.url + path);
  }

  /** Runs axe-core and fails on serious or critical violations. */
  async checkA11y(label: string) {
    await this.settle();
    await this.page.evaluate(await axeSource());
    const violations = await this.page.evaluate(async () => {
      // biome-ignore lint/suspicious/noExplicitAny: axe is injected at runtime
      const axe = (window as any).axe;
      const r = await axe.run(document, { resultTypes: ["violations"] });
      return (
        r.violations as {
          id: string;
          impact: string;
          help: string;
          nodes: { html: string; failureSummary: string }[];
        }[]
      )
        .filter((v) => v.impact === "serious" || v.impact === "critical")
        .flatMap((v) =>
          v.nodes.map((n) => `${v.impact} ${v.id}: ${v.help}\n  ${n.html}\n  ${n.failureSummary}`),
        );
    });
    if (violations.length)
      throw new Error(`axe (${label}, ${this.viewport.name}):\n${violations.join("\n")}`);
  }

  /**
   * Waits for the steady state axe should judge: running CSS transitions (a colour-scheme switch
   * animates colours) and react-aria's live announcements, which it removes after 7 s and which can
   * still point at a button that has unmounted. Both are waited out, not excluded: whatever is
   * still there after the wait is checked.
   */
  private async settle() {
    await this.page.evaluate(() =>
      Promise.all(
        document
          .getAnimations()
          .filter((a) => a instanceof CSSTransition)
          .map((a) => a.finished.catch(() => {})),
      ),
    );
    await this.page
      .waitForFunction(
        () => !document.querySelector("[data-live-announcer] [role=log] > *"),
        null,
        {
          timeout: 8000,
        },
      )
      .catch(() => {});
  }

  async assertNoHorizontalOverflow(label: string) {
    const over = await this.page.evaluate(() => {
      const width = document.documentElement.clientWidth;
      if (document.documentElement.scrollWidth <= width) return null;
      const wide = [...document.querySelectorAll("body *")]
        .filter((el) => el.getBoundingClientRect().right > width + 1)
        .slice(0, 5)
        .map((el) => `${el.tagName.toLowerCase()}.${el.className}`);
      return `scrollWidth ${document.documentElement.scrollWidth} > ${width}; ${wide.join(", ")}`;
    });
    if (over) throw new Error(`horizontal overflow (${label}, ${this.viewport.name}): ${over}`);
  }

  screenshot(label: string, scheme: string) {
    return this.page.screenshot({
      path: join(artifactsDir, `${label}-${this.viewport.width}-${scheme}.png`),
      fullPage: true,
    });
  }

  /** axe, overflow and a screenshot in light and dark. */
  async inspect(label: string) {
    for (const scheme of ["light", "dark"] as const) {
      await this.page.emulateMedia({ colorScheme: scheme });
      await this.checkA11y(`${label} ${scheme}`);
      await this.assertNoHorizontalOverflow(`${label} ${scheme}`);
      await this.screenshot(label, scheme);
    }
    await this.page.emulateMedia({ colorScheme: "light" });
  }
}

let axe: Promise<string> | undefined;
function axeSource(): Promise<string> {
  // Evaluated through the DevTools protocol, not a <script> tag, so the page's CSP is untouched.
  axe ??= Bun.file(join(import.meta.dir, "../node_modules/axe-core/axe.min.js")).text();
  return axe;
}

function pathOf(url: string): string {
  try {
    return new URL(url).pathname;
  } catch {
    return "";
  }
}

/**
 * Runs `fn` on a fresh page and fails at the end on any console error, page error or
 * undeclared API error status (each listed with its text and location).
 */
export async function scenario(
  server: Server,
  viewport: Viewport,
  fn: (s: Session) => Promise<void>,
  /** Extra context options, for example `hasTouch` for a coarse pointer. */
  extra: BrowserContextOptions = {},
) {
  const context = await (await getBrowser()).newContext({
    viewport: { width: viewport.width, height: viewport.height },
    colorScheme: "light",
    acceptDownloads: true,
    ...extra,
  });
  context.setDefaultTimeout(30_000);
  const session = new Session(await context.newPage(), server, viewport);
  let error: unknown;
  try {
    await fn(session);
  } catch (err) {
    error = err;
  }
  // Late events (an SSE update, a last fetch) still count.
  await session.page.waitForTimeout(250).catch(() => {});
  await context.close();
  const failures = session.failures();
  if (error === undefined && failures.length === 0) return;
  const rules = failures.length ? `\nscenario rule failures:\n${failures.join("\n")}` : "";
  if (error instanceof Error) {
    error.message += rules;
    throw error;
  }
  throw new Error(`${error === undefined ? "" : String(error)}${rules}`);
}
