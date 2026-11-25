// Browser end-to-end scenarios against the real binary in sim mode, at three viewports, plus the
// multi-device scenarios on the office and standalone-scanner fixtures.
// Every scenario runs under the harness's console and API status rules.
import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { mkdtempSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import type { Locator, Page } from "playwright-core";
import { devicePickerStrings } from "../src/components/DevicePicker.strings";
import { eulaStrings } from "../src/components/EulaAccept.strings";
import { optionFieldStrings } from "../src/components/OptionField.strings";
import { optionLabel, optionTitle } from "../src/lib/labels";
import { labelStrings } from "../src/lib/labels.strings";
import { strings } from "../src/strings";
import { printStrings } from "../src/views/Print.strings";
import { scanStrings } from "../src/views/Scan.strings";
import { settingsStrings } from "../src/views/Settings.strings";
import { setupStrings } from "../src/views/Setup.strings";
import { statusStrings } from "../src/views/Status.strings";
import {
  closeBrowser,
  repoRoot,
  type Server,
  type Session,
  scenario,
  startServer,
  type Viewport,
} from "./harness";

const viewports: Viewport[] = [
  { name: "360x800", width: 360, height: 800 },
  { name: "768x1024", width: 768, height: 1024 },
  { name: "1440x900", width: 1440, height: 900 },
];
const desktop = viewports[2] as Viewport;
/** The multi-device Status is also checked on a wide screen. */
const statusViewports: Viewport[] = [
  ...viewports,
  { name: "1920x1080", width: 1920, height: 1080 },
];

const samplePdf = join(repoRoot, "internal/printing/testdata/sample.pdf");
// A 1×1 PNG, which the simulated printer (PDF and JPEG, no PNG) gets converted to JPEG.
const tmp = mkdtempSync(join(tmpdir(), "pagewire-e2e-files-"));
const samplePng = join(tmp, "dot.png");
await Bun.write(
  samplePng,
  Buffer.from(
    "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==",
    "base64",
  ),
);
// Not a real .docx, but the same ZIP signature: refused, with the Word help.
const sampleDocx = join(tmp, "letter.docx");
await Bun.write(sampleDocx, Buffer.from("PK\x03\x04\x14\x00\x06\x00", "binary"));

type FixtureDevice = { product: string; vendorId: string; productId: string };
const fixtureDevices = async (name: string): Promise<FixtureDevice[]> =>
  (await Bun.file(join(repoRoot, `internal/catalog/fixtures/${name}.json`)).json()).devices;
const fixtureProduct = async (name: string): Promise<string> =>
  ((await fixtureDevices(name))[0] as FixtureDevice).product;

type StatusEntry = {
  id: string;
  name: string;
  enabled: boolean;
  phase: string;
  decision: { device: unknown } | null;
};
type StatusJSON = {
  phase: string;
  config: { state: string };
  printers: StatusEntry[];
  scanners: StatusEntry[];
  workloads: { mode: string }[];
};

async function apiStatus(server: Server): Promise<StatusJSON> {
  return (await fetch(`${server.url}/api/v1/status`)).json() as Promise<StatusJSON>;
}

const brief = (s: StatusJSON | undefined) =>
  s &&
  JSON.stringify({
    phase: s.phase,
    printers: s.printers.map((p) => [p.id, p.phase]),
    scanners: s.scanners.map((x) => [x.id, x.phase]),
  });

/** Polls Status every second until `ok` holds; reports the last Status on timeout. */
async function waitForStatus(server: Server, ok: (s: StatusJSON) => boolean, timeoutMs = 120_000) {
  const deadline = Date.now() + timeoutMs;
  let last: StatusJSON | undefined;
  while (Date.now() < deadline) {
    last = await apiStatus(server);
    if (ok(last)) return last;
    await Bun.sleep(1000);
  }
  throw new Error(`Status did not reach the expected state: ${brief(last)}`);
}

type SetupJSON = {
  suggested: Record<string, unknown>;
  devices: { suggestedPrinter: unknown | null; suggestedScanner: unknown | null }[];
};

/**
 * Creates the config from every device suggestion in setup through the API, skipping the wizard,
 * and waits for the overall `phase`: a device whose required licence is not accepted yet stays
 * degraded.
 */
async function seedConfig(server: Server, phase = "ready") {
  const deadline = Date.now() + 60_000;
  let setup: SetupJSON;
  // The suggestions follow the first probe report.
  for (;;) {
    setup = (await (await fetch(`${server.url}/api/v1/setup`)).json()) as SetupJSON;
    if (setup.devices.some((d) => d.suggestedPrinter || d.suggestedScanner)) break;
    if (Date.now() > deadline)
      throw new Error(`setup suggests no device: ${JSON.stringify(setup)}`);
    await Bun.sleep(500);
  }
  const config = {
    ...setup.suggested,
    printers: setup.devices.flatMap((d) => (d.suggestedPrinter ? [d.suggestedPrinter] : [])),
    scanners: setup.devices.flatMap((d) => (d.suggestedScanner ? [d.suggestedScanner] : [])),
  };
  const r = await fetch(`${server.url}/api/v1/config`, {
    method: "PUT",
    headers: { "Content-Type": "application/json", "If-None-Match": "*" },
    body: JSON.stringify(config),
  });
  if (r.status !== 201) throw new Error(`seeding the config: ${r.status} ${await r.text()}`);
  await waitForStatus(server, (s) => s.phase === phase);
}

const nav = (page: Page, label: string) =>
  page.getByRole("navigation", { name: strings.nav.label }).getByRole("link", { name: label });

const heading = (page: Page, name: string) =>
  page.getByRole("heading", { level: 1, name, exact: true });

/** A Settings entry: its section is labelled by the entry's name. */
const entry = (page: Page, name: string) => page.getByRole("region", { name, exact: true });

/** A card, by its title. */
const card = (page: Page, title: string) =>
  page
    .locator(".ui-card")
    .filter({ has: page.locator(".ui-card-title").getByText(title, { exact: true }) });

/** A form field (Select, NumberField, the device picker) by its exact label. */
const field = (page: Page, title: string, scope: Page | Locator = page) =>
  scope
    .locator(".ui-field")
    .filter({ has: page.locator(".ui-label").getByText(title, { exact: true }) });

const trigger = (page: Page, title: string, scope: Page | Locator = page) =>
  field(page, title, scope).locator(".ui-select-trigger");

/** The text of every element the trigger's aria-describedby names. */
const describedText = (l: Locator) =>
  l.evaluate((el) =>
    (el.getAttribute("aria-describedby") ?? "")
      .split(/\s+/)
      .filter(Boolean)
      .map((id) => document.getElementById(id)?.textContent ?? "")
      .join(" "),
  );

/** Polls `get` until `ok` holds; the caps behind a control arrive with Status. */
async function until<T>(what: string, get: () => Promise<T>, ok: (v: T) => boolean) {
  const deadline = Date.now() + 30_000;
  let last: T | undefined;
  while (Date.now() < deadline) {
    last = await get();
    if (ok(last)) return last;
    await Bun.sleep(250);
  }
  throw new Error(`${what}: last value ${JSON.stringify(last)}`);
}

/** A disabled option control whose accessible description is `reason`. */
async function expectDisabledWithReason(page: Page, scope: Locator, title: string, reason: string) {
  const t = trigger(page, title, scope);
  await t.waitFor();
  await until(
    `${title}'s description`,
    () => describedText(t),
    (v) => v.includes(reason),
  );
  expect(await t.isDisabled()).toBe(true);
}

/** The labels of the options a Select offers, read from its open listbox. */
async function optionsOf(page: Page, title: string, scope: Page | Locator = page) {
  await trigger(page, title, scope).click();
  const list = page.getByRole("listbox");
  await list.waitFor();
  const labels = (await list.getByRole("option").allInnerTexts()).map((x) => x.trim());
  await page.keyboard.press("Escape");
  await list.waitFor({ state: "hidden" });
  return labels;
}

const optionTitles = new Set<string>(Object.values(labelStrings.titles));

/**
 * A24: the option control is a listbox button, and no option kind is a radio group (the device
 * picker and Settings' connection choice are radio groups, but not option kinds).
 */
async function expectSharedControl(page: Page, title: string) {
  const t = trigger(page, title).first();
  await t.waitFor();
  expect(await t.getAttribute("aria-haspopup")).toBe("listbox");
  const groups = await page.evaluate(() =>
    [...document.querySelectorAll("[role=radiogroup]")].map(
      (g) =>
        g.getAttribute("aria-label") ??
        (g.getAttribute("aria-labelledby") ?? "")
          .split(/\s+/)
          .map((id) => document.getElementById(id)?.textContent ?? "")
          .join(" ")
          .trim(),
    ),
  );
  expect(groups.filter((g) => optionTitles.has(g))).toEqual([]);
}

const PWG_KEYWORD = /\b[a-z]+_[a-z0-9.-]+_[0-9.]+x[0-9.]+(mm|in)\b/;
const RAW_KEYWORDS = new Set([
  "one-sided",
  "two-sided-long-edge",
  "monochrome",
  "platen",
  "adf",
  "tray-1",
  "face-down",
]);

/** R2: after opening every option listbox, no visible text is a raw PWG or IPP keyword. */
async function expectNoRawKeywords(page: Page, label: string) {
  // biome-ignore lint/performance/useDomNodeTextContent: the rendered text is what people see; hidden nodes must not count
  const texts = [await page.locator("body").innerText()];
  const triggers = page.locator("main .ui-select-trigger:not([disabled]):visible");
  const n = await triggers.count();
  for (let i = 0; i < n; i++) {
    await triggers.nth(i).click();
    const list = page.getByRole("listbox");
    await list.waitFor();
    // biome-ignore lint/performance/useDomNodeTextContent: the rendered text is what people see; hidden nodes must not count
    texts.push(await list.innerText());
    await page.keyboard.press("Escape");
    await list.waitFor({ state: "hidden" });
  }
  const all = texts.join("\n");
  expect({ label, pwg: all.match(PWG_KEYWORD)?.[0] ?? null }).toEqual({ label, pwg: null });
  const raw = all
    .split("\n")
    .map((l) => l.trim())
    .filter((l) => RAW_KEYWORDS.has(l));
  expect({ label, raw }).toEqual({ label, raw: [] });
}

/** The accessible-ish name of the focused element: aria-label, labelledby, label or text. */
function focusedName(page: Page): Promise<string> {
  return page.evaluate(() => {
    const el = document.activeElement as HTMLElement | null;
    if (!el) return "";
    const byIds = (ids: string | null) =>
      (ids ?? "")
        .split(/\s+/)
        .map((id) => document.getElementById(id)?.textContent ?? "")
        .join(" ");
    const input = el as HTMLInputElement;
    return (
      el.getAttribute("aria-label") ??
      (el.getAttribute("aria-labelledby")
        ? byIds(el.getAttribute("aria-labelledby"))
        : input.labels?.[0]?.textContent) ??
      el.textContent ??
      ""
    ).trim();
  });
}

/** Presses Tab until the focused element's name matches, without touching the mouse. */
async function tabTo(page: Page, name: string | RegExp, max = 80) {
  const seen: string[] = [];
  for (let i = 0; i < max; i++) {
    await page.keyboard.press("Tab");
    const got = await focusedName(page);
    if (typeof name === "string" ? got === name : name.test(got)) return;
    seen.push(got);
  }
  throw new Error(`Tab never reached ${name}; focus went through: ${seen.join(" | ")}`);
}

const escapeRe = (s: string) => s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
/** The defaults step saves the locale's paper size when the printer offers it, else it keeps. */
const defaultsAction = new RegExp(
  `^(${escapeRe(setupStrings.defaults.keep)}|${escapeRe(setupStrings.defaults.save)})$`,
);

type WizardOptions = {
  /** Screenshot name prefix, so two fixtures at one width keep their own files. */
  prefix?: string;
  /** Inspect (axe, overflow, screenshots) every step; else only the defaults step. */
  inspectSteps?: boolean;
  /** Runs on the defaults step, before it is left. */
  atDefaults?: (s: Session) => Promise<void>;
};

/**
 * Pointer path through the wizard: devices, names, drivers, review, then Save, the Applying
 * screen, the defaults step and the done screen, checking each step on the way.
 */
async function runWizard(
  s: Session,
  { prefix = "setup", inspectSteps = true, atDefaults }: WizardOptions = {},
) {
  const { page } = s;
  const next = page.getByRole("button", { name: strings.buttons.next, exact: true });
  for (const step of ["devices", "names", "drivers"] as const) {
    await heading(page, setupStrings.steps[step]).waitFor();
    if (inspectSteps) await s.inspect(`${prefix}-${step}`);
    await next.click();
  }
  await heading(page, setupStrings.steps.review).waitFor();
  if (inspectSteps) await s.inspect(`${prefix}-review`);
  await page.getByRole("button", { name: setupStrings.review.finish, exact: true }).click();
  await heading(page, setupStrings.steps.defaults).waitFor({ timeout: 180_000 });
  await s.inspect(`${prefix}-defaults`);
  await atDefaults?.(s);
  await page.getByRole("button", { name: defaultsAction }).click();
  await heading(page, setupStrings.done.title).waitFor();
  if (inspectSteps) await s.inspect(`${prefix}-done`);
}

/** Picks an option in a react-aria Select by its label. */
async function choose(page: Page, label: string, option: string) {
  await trigger(page, label).click();
  await page.getByRole("option", { name: option, exact: true }).click();
}

/** Opens every collapsed disclosure with this title. */
async function expandAll(page: Page, title: string, count: number) {
  const triggers = page.getByRole("button", { name: title, exact: true });
  await triggers.nth(count - 1).waitFor();
  expect(await triggers.count()).toBe(count);
  for (let i = 0; i < count; i++) {
    const t = triggers.nth(i);
    if ((await t.getAttribute("aria-expanded")) !== "true") await t.click();
  }
}

/** A device picker choice (segments for up to four devices), by the device's name. */
const pickerChoice = (page: Page, kind: "printer" | "scanner", name: string) =>
  page
    .getByRole("radiogroup", { name: devicePickerStrings[kind], exact: true })
    .getByRole("radio", { name: new RegExp(`^${escapeRe(name)}\\b`) });

afterAll(closeBrowser);

for (const vp of viewports) {
  describe(vp.name, () => {
    let main: Server;
    beforeAll(async () => {
      main = await startServer(`main-${vp.width}`);
    });
    afterAll(() => main?.stop());

    test(`1. wizard with the ippusb-mfp fixture reaches ready (${vp.name})`, async () => {
      const product = await fixtureProduct("ippusb-mfp");
      await scenario(main, vp, async (s) => {
        await s.goto("/");
        await s.page.waitForURL(/\/setup$/);
        await s.page.getByText(product).waitFor();
        await runWizard(s);
      });
      const st = await apiStatus(main);
      expect(st.phase).toBe("ready");
      expect(st.workloads.map((w) => w.mode)).toEqual(["usb"]);
      expect(st.printers.map((p) => p.phase)).toEqual(["ready"]);
      expect(st.scanners.map((x) => x.phase)).toEqual(["ready"]);
    });

    test(`2. Status shows connect URIs (${vp.name})`, async () => {
      await scenario(main, vp, async (s) => {
        await s.goto("/");
        await heading(s.page, strings.nav.status).waitFor();
        // One printer card and one scanner card, each with its own addresses.
        await expandAll(s.page, statusStrings.connect.title, 2);
        await s.page
          .locator("code", { hasText: /^ipp:\/\/127\.0\.0\.1:\d+\// })
          .first()
          .waitFor();
        await s.page
          .locator("code", { hasText: /^https?:\/\/127\.0\.0\.1:\d+\// })
          .first()
          .waitFor();
      });
    });

    test(`3. print: the job row reaches completed without a reload (${vp.name})`, async () => {
      await scenario(main, vp, async (s) => {
        const { page } = s;
        await s.goto("/");
        await heading(page, strings.nav.status).waitFor();
        await nav(page, strings.nav.print).click();
        await heading(page, strings.nav.print).waitFor();
        // A marker that a reload would wipe, and a second SSE listener that proves the completed
        // state arrives as a `job` event (useJobs() could also get it from a refetch).
        await page.evaluate(
          () =>
            new Promise<void>((opened) => {
              const w = window as unknown as { e2eMarker: boolean; e2eJob: Promise<void> };
              w.e2eMarker = true;
              const es = new EventSource("/api/v1/events");
              w.e2eJob = new Promise<void>((resolve) =>
                es.addEventListener("job", (e) => {
                  const job = JSON.parse((e as MessageEvent).data);
                  if (job.name === "sample.pdf" && job.state === "completed") {
                    es.close();
                    resolve();
                  }
                }),
              );
              es.onopen = () => opened();
            }),
        );
        await page.locator("input[type=file]").setInputFiles(samplePdf);
        await page.getByText(printStrings.files.ready).waitFor();
        await page.getByRole("button", { name: printStrings.submit(1, 1, 1), exact: true }).click();
        await page
          .getByRole("listitem")
          .filter({ hasText: "sample.pdf" })
          .filter({ hasText: printStrings.jobs.state.completed })
          .first()
          .waitFor({ timeout: 60_000 });
        await page.waitForFunction(
          () => (window as unknown as { e2eJob: Promise<void> }).e2eJob.then(() => true),
          null,
          { timeout: 30_000 },
        );
        const kept = await page.evaluate(
          () => (window as unknown as { e2eMarker?: boolean }).e2eMarker === true,
        );
        expect(kept).toBe(true);
      });
    });

    test(`12. print: a PNG is converted, a .docx gets help, the button counts files (${vp.name})`, async () => {
      await scenario(main, vp, async (s) => {
        const { page } = s;
        await s.goto("/print");
        await heading(page, strings.nav.print).waitFor();
        await page.getByText(/This printer accepts .*PNG/).waitFor();
        await page.locator("input[type=file]").setInputFiles([samplePng, sampleDocx]);
        const docx = page.getByRole("listitem").filter({ hasText: "letter.docx" });
        await docx.getByText(printStrings.help.document).waitFor();
        await page
          .getByRole("listitem")
          .filter({ hasText: "dot.png" })
          .getByText(printStrings.files.ready)
          .waitFor();
        const submit = page.getByRole("button", {
          name: printStrings.submit(1, 2, 1),
          exact: true,
        });
        await submit.waitFor();
        await s.inspect("print-files");
        await submit.click();
        await page
          .getByRole("listitem")
          .filter({ hasText: "dot.png" })
          .filter({ hasText: printStrings.jobs.state.completed })
          .first()
          .waitFor({ timeout: 60_000 });
      });
    });

    test(`4. scan: two pages, preview and PDF download (${vp.name})`, async () => {
      await scenario(main, vp, async (s) => {
        const { page } = s;
        await s.goto("/");
        await heading(page, strings.nav.status).waitFor();
        await nav(page, strings.nav.scan).click();
        await heading(page, strings.nav.scan).waitFor();
        await choose(
          page,
          optionTitle("source", "scanner"),
          optionLabel("source", "adf", "scanner"),
        );
        await page.getByRole("button", { name: scanStrings.submit, exact: true }).click();
        const download = page.getByRole("link", { name: scanStrings.download.pdf });
        await download.waitFor({ timeout: 60_000 });
        await page.getByText(scanStrings.history.summary(2, "PDF")).first().waitFor();

        await page.getByRole("button", { name: scanStrings.result.openPage(1) }).click();
        const img = page.getByRole("dialog").getByRole("img", { name: scanStrings.result.page(1) });
        await img.waitFor();
        await page.waitForFunction(
          () => (document.querySelector("[role=dialog] img") as HTMLImageElement | null)?.complete,
        );
        expect(await img.evaluate((i) => (i as HTMLImageElement).naturalWidth)).toBeGreaterThan(0);
        await s.inspect("scan-preview");
        await page.keyboard.press("Escape");
        await page.getByRole("dialog").waitFor({ state: "hidden" });

        const [file] = await Promise.all([page.waitForEvent("download"), download.click()]);
        const path = await file.path();
        const head = await Bun.file(path).slice(0, 5).text();
        expect(head).toBe("%PDF-");
      });
    });

    test(`5. settings conflict: keep mine after an edit on disk (${vp.name})`, async () => {
      const product = await fixtureProduct("ippusb-mfp");
      const location = `Office ${vp.width}`;
      const comment = "# edited on disk by the e2e test";
      await scenario(main, vp, async (s) => {
        const { page } = s;
        // The file changes under the open form, so the save gets a 412 by design.
        s.expectApiStatus("PATCH", "/api/v1/config", 412);
        await s.goto("/settings");
        const printer = entry(page, product);
        await printer.getByRole("button", { name: settingsStrings.edit, exact: true }).click();
        const field = printer.getByRole("textbox", { name: settingsStrings.fields.location });
        await field.fill(location);

        // Unsaved edits: Save is in view without scrolling, above the phone tab bar.
        await page.getByText(settingsStrings.unsaved).waitFor();
        const save = await page
          .getByRole("button", { name: strings.buttons.save, exact: true })
          .boundingBox();
        const tabBar = vp.width < 768 ? 64 : 0;
        expect(save).not.toBeNull();
        expect((save?.y ?? 0) + (save?.height ?? 0)).toBeLessThanOrEqual(vp.height - tabBar);
        // Leaving asks first; Keep editing keeps the edit.
        await nav(page, strings.nav.print).click();
        const leave = page.getByRole("alertdialog", { name: settingsStrings.leave.title });
        await leave.waitFor();
        await s.inspect("settings-leave");
        await leave.getByRole("button", { name: settingsStrings.leave.stay }).click();
        await leave.waitFor({ state: "hidden" });
        expect(new URL(page.url()).pathname).toBe("/settings");
        expect(await field.inputValue()).toBe(location);

        const before = await Bun.file(main.configPath).text();
        const edited = `${comment}\n${before.replace(/retentionDays: \d+/, "retentionDays: 14")}`;
        expect(edited).toContain("retentionDays: 14");
        await Bun.write(main.configPath, edited);
        await page.getByText(settingsStrings.fileChanged).waitFor();

        await page.getByRole("button", { name: strings.buttons.save, exact: true }).click();
        await page.getByRole("dialog", { name: settingsStrings.conflict.title }).waitFor();
        await s.inspect("settings-conflict");
        await page.getByRole("button", { name: settingsStrings.conflict.keepMine }).click();
        await page.getByText(settingsStrings.saved).first().waitFor();

        const after = await Bun.file(main.configPath).text();
        expect(after).toContain(comment);
        expect(after).toContain("retentionDays: 14");
        expect(after).toMatch(new RegExp(`location: "?${location}"?\\n`));

        await page.reload();
        const reloaded = entry(page, product);
        await reloaded.getByRole("button", { name: settingsStrings.edit, exact: true }).click();
        await reloaded
          .getByRole("textbox", { name: settingsStrings.fields.location })
          .and(page.locator(`[value="${location}"]`))
          .waitFor();
      });
    });

    test(`6. an invalid file shows the global banner with a line (${vp.name})`, async () => {
      const valid = await Bun.file(main.configPath).text();
      try {
        await scenario(main, vp, async (s) => {
          const { page } = s;
          await s.goto("/");
          await heading(page, strings.nav.status).waitFor();
          await Bun.write(main.configPath, `${valid}notAKey: 1\n`);
          const banner = page.getByRole("alert").filter({ hasText: strings.banners.invalidConfig });
          await banner.waitFor();
          await banner.getByText(/^Line \d+: /).waitFor();
          await s.inspect("status-invalid");
          await Bun.write(main.configPath, valid);
          await banner.waitFor({ state: "hidden" });
        });
      } finally {
        await Bun.write(main.configPath, valid);
      }
      await waitForStatus(main, (st) => st.config.state === "applied");
    });

    test(`10. axe, overflow and screenshots on every page in light and dark (${vp.name})`, async () => {
      const pages: [string, string, string][] = [
        ["/", "status", strings.nav.status],
        ["/print", "print", strings.nav.print],
        ["/scan", "scan", strings.nav.scan],
        ["/settings", "settings", strings.nav.settings],
        ["/setup", "setup-already-done", setupStrings.alreadyDone.title],
      ];
      await scenario(main, vp, async (s) => {
        for (const [path, label, title] of pages) {
          await s.goto(path);
          await heading(s.page, title).waitFor();
          // Let the first Status event and the page's own fetches render.
          await s.page.waitForTimeout(750);
          await s.inspect(label);
        }
      });
    });

    test(`8. keyboard-only path through the wizard (${vp.name})`, async () => {
      const server = await startServer(`keyboard-${vp.width}`);
      try {
        const product = await fixtureProduct("ippusb-mfp");
        await scenario(server, vp, async (s) => {
          const { page } = s;
          // Late frames, as on a loaded CI runner: a focus move the app defers to the next frame
          // then lands after the keys below, and must not take focus the keyboard already moved.
          await page.addInitScript(() => {
            const raf = window.requestAnimationFrame.bind(window);
            const caf = window.cancelAnimationFrame.bind(window);
            const late = new Map<number, number>();
            window.requestAnimationFrame = (cb) => {
              const id = raf((t) => {
                late.set(
                  id,
                  window.setTimeout(() => late.delete(id) && cb(t), 50),
                );
              });
              return id;
            };
            window.cancelAnimationFrame = (id) => {
              caf(id);
              clearTimeout(late.get(id));
              late.delete(id);
            };
          });
          await s.goto("/setup");
          await page.getByText(product).waitFor();
          await tabTo(page, strings.buttons.next);
          await page.keyboard.press("Enter");
          await heading(page, setupStrings.steps.names).waitFor();
          // The installation name comes first; the printer's name field follows.
          await tabTo(page, new RegExp(`^${setupStrings.names.nameLabel}`));
          await page.keyboard.press("ControlOrMeta+a");
          await page.keyboard.type("Keyboard printer");
          for (const step of ["drivers", "review"] as const) {
            await tabTo(page, strings.buttons.next);
            await page.keyboard.press("Enter");
            await heading(page, setupStrings.steps[step]).waitFor();
          }
          await tabTo(page, setupStrings.review.finish);
          await page.keyboard.press("Enter");
          await heading(page, setupStrings.steps.defaults).waitFor({ timeout: 180_000 });
          await tabTo(page, defaultsAction);
          await page.keyboard.press("Enter");
          await heading(page, setupStrings.done.title).waitFor();
          await tabTo(page, setupStrings.done.goToStatus);
          await page.keyboard.press("Enter");
          await heading(page, strings.nav.status).waitFor();
        });
        const cfg = await Bun.file(server.configPath).text();
        expect(cfg).toContain("name: Keyboard printer");
      } finally {
        await server.stop();
      }
    });

    test(`7. EULA flow: accepting in Settings shows the blob installed (${vp.name})`, async () => {
      const server = await startServer(`hp-${vp.width}`, "hp-legacy-mfp");
      try {
        // hplip marks this model's driver as needing the plug-in, so the device waits for it.
        await seedConfig(server, "degraded");
        const setup = (await (await fetch(`${server.url}/api/v1/setup`)).json()) as {
          eulas: { id: string; title: string; version: string; textSha256: string | null }[];
        };
        const eula = setup.eulas.find((e) => e.id === "hplip-plugin");
        if (!eula) throw new Error(`setup lists no hplip-plugin EULA: ${JSON.stringify(setup)}`);
        const product = await fixtureProduct("hp-legacy-mfp");
        await scenario(server, vp, async (s) => {
          const { page } = s;
          // Accepting adds the blob, which rolls the device; jobs are 503 until the printer is back.
          s.expectApiStatus("GET", /^\/api\/v1\/printers\/[^/]+\/jobs$/, 503);
          await s.goto("/settings");
          // The licence text from inside the verified archive, in a named, focusable region.
          const text = page.getByRole("region", { name: eulaStrings.textLabel(eula.title) });
          await text.waitFor();
          expect((await text.textContent())?.length).toBeGreaterThan(0);
          expect(await text.getAttribute("tabindex")).toBe("0");
          await page.getByText(eulaStrings.accept(eula.title, eula.version)).click();
          await page.getByText(settingsStrings.unsaved).waitFor();
          await s.inspect("settings-eula-unsaved");
          await page.getByRole("button", { name: strings.buttons.save, exact: true }).click();
          await page.getByText(settingsStrings.saved).first().waitFor();
          const cfg = await Bun.file(server.configPath).text();
          expect(cfg).toMatch(new RegExp(`textSha256: "?${eula.textSha256}"?\n`));
          await nav(page, strings.nav.status).click();
          await heading(page, strings.nav.status).waitFor();
          // The vendor files are listed in the printer card's technical details.
          const printer = page.locator(".status-device").filter({ hasText: product });
          await printer.getByRole("button", { name: strings.technicalDetails }).click();
          await printer.getByText(statusStrings.device.blobsTitle).waitFor({ timeout: 90_000 });
          await printer
            .getByText(statusStrings.device.blobState.installed, { exact: true })
            .first()
            .waitFor({ timeout: 90_000 });
          await s.inspect("status-blob");
        });
      } finally {
        await server.stop();
      }
    });

    test(`9. no USB: the simulated printer reaches ready (${vp.name})`, async () => {
      const server = await startServer(`none-${vp.width}`, "none");
      try {
        await scenario(server, vp, async (s) => {
          const { page } = s;
          await s.goto("/setup");
          await page.getByText(setupStrings.detect.noUsbTitle).waitFor();
          await s.inspect("setup-no-usb");
          await page.getByRole("button", { name: setupStrings.detect.useSimulated }).click();
          await page.getByText(setupStrings.detect.simulatedTitle).waitFor();
          await runWizard(s, { prefix: "simulated-setup" });
          await page.getByRole("link", { name: setupStrings.done.goToStatus }).click();
          await heading(page, strings.nav.status).waitFor();
          const printer = page.locator(".status-device").first();
          await printer.getByRole("button", { name: strings.technicalDetails }).click();
          await printer.getByText(statusStrings.device.simulatedDevice, { exact: true }).waitFor();
          expect(await page.getByText(strings.problems["bad-response"]).count()).toBe(0);
          await s.inspect("status-simulated");
        });
        const st = await waitForStatus(server, (x) => x.phase === "ready");
        expect(st.workloads.map((w) => w.mode)).toEqual(["simulated"]);
        expect(st.printers.map((p) => p.decision?.device)).toEqual([null]);
      } finally {
        await server.stop();
      }
    });
  });
}

test("13. touch screens see no Drop files here", async () => {
  const server = await startServer("touch");
  try {
    await seedConfig(server);
    await scenario(
      server,
      viewports[0] as Viewport,
      async (s) => {
        await s.goto("/print");
        await heading(s.page, strings.nav.print).waitFor();
        await s.page.getByRole("button", { name: printStrings.files.choose }).waitFor();
        expect(await s.page.getByText(printStrings.files.drop).isVisible()).toBe(false);
        await s.inspect("print-touch");
      },
      { hasTouch: true, isMobile: true },
    );
  } finally {
    await server.stop();
  }
});

test("14. scan: Cancel stops a running scan", async () => {
  // Each page takes a minute, so the scan is still running when Cancel is pressed.
  const server = await startServer("scan-cancel", "ippusb-mfp", ["--sim-scan-delay=60s"]);
  try {
    await seedConfig(server);
    await scenario(server, viewports[1] as Viewport, async (s) => {
      const { page } = s;
      await s.goto("/scan");
      await heading(page, strings.nav.scan).waitFor();
      await page.getByRole("button", { name: scanStrings.submit, exact: true }).click();

      const cancel = page.getByRole("button", { name: scanStrings.cancel });
      await cancel.waitFor();
      await page.getByRole("progressbar", { name: scanStrings.scanning(0) }).waitFor();
      expect(await page.getByText(scanStrings.history.summary(0, "PDF")).count()).toBe(0);
      await s.inspect("scan-running");

      const deleted = page.waitForResponse(
        (r) => r.request().method() === "DELETE" && /\/api\/v1\/scans\/\w+$/.test(r.url()),
      );
      await cancel.click();
      expect((await deleted).status()).toBe(202);
      await page.getByText(scanStrings.result.canceled).waitFor({ timeout: 15_000 });
      await cancel.waitFor({ state: "hidden" });
      expect(await page.getByRole("link", { name: scanStrings.download.pdf }).count()).toBe(0);
      await s.inspect("scan-canceled");

      // The scanner is free again: a new scan starts and can be canceled too.
      await page.getByRole("button", { name: scanStrings.submit, exact: true }).click();
      await cancel.waitFor();
      const again = page.waitForResponse((r) => r.request().method() === "DELETE");
      await cancel.click();
      expect((await again).status()).toBe(202);
      await cancel.waitFor({ state: "hidden", timeout: 15_000 });
    });
    const scans = (await (await fetch(`${server.url}/api/v1/scans`)).json()) as {
      state: string;
    }[];
    expect(scans.map((x) => x.state)).toEqual(["canceled", "canceled"]);
  } finally {
    await server.stop();
  }
});

test("11. self-test: the console and API status rules report failures", async () => {
  const server = await startServer("self-test");
  try {
    await seedConfig(server);
    await scenario(server, viewports[2] as Viewport, async (s) => {
      const { page } = s;
      await s.goto("/");
      await heading(page, strings.nav.status).waitFor();

      const logged = page.waitForEvent("console", (m) => m.text() === "probe");
      await page.evaluate(() => console.error("probe"));
      await logged;
      expect(s.failures()).toEqual([expect.stringContaining("console.error: probe")]);
      s.clear();

      const thrown = page.waitForEvent("pageerror");
      await page.evaluate(() => {
        setTimeout(() => {
          throw new Error("probe-throw");
        });
      });
      await thrown;
      expect(s.failures()).toEqual([expect.stringContaining("probe-throw")]);
      s.clear();

      // An API 404 makes Chromium log "Failed to load resource". That line is excluded from
      // the console rule; the API status rule reports the response instead.
      const missing = page.waitForResponse((r) =>
        r.url().endsWith("/api/v1/scans/0000000000000000"),
      );
      const loadFailed = page.waitForEvent("console", (m) =>
        m.text().startsWith("Failed to load resource: the server responded with a status of 404"),
      );
      await page.evaluate(() => fetch("/api/v1/scans/0000000000000000"));
      await Promise.all([missing, loadFailed]);
      expect(s.failures()).toEqual([
        "unexpected API status: GET /api/v1/scans/0000000000000000 404",
      ]);
      s.expectApiStatus("GET", /^\/api\/v1\/scans\/0+$/, 404);
      expect(s.failures()).toEqual([]);
      s.clear();
    });

    // The scenario wrapper itself turns a recorded console error into a failing scenario.
    const wrapped = scenario(server, viewports[2] as Viewport, async (s) => {
      await s.goto("/");
      await heading(s.page, strings.nav.status).waitFor();
      await s.page.evaluate(() => console.error("probe"));
    });
    await expect(wrapped).rejects.toThrow(/scenario rule failures:\nconsole\.error: probe/);
  } finally {
    await server.stop();
  }
});

// The office fixture: an HP OfficeJet IPP-over-USB MFP, a Samsung SCX-4300 (legacy printer
// application), a CanoScan LiDE 100 (scanner only) and a USB keyboard (neither).
describe("office fixture", () => {
  let office: Server;
  const p = optionFieldStrings;
  const title = (
    kind: Parameters<typeof optionTitle>[0],
    role: "printer" | "scanner" = "printer",
  ) => optionTitle(kind, role);
  let hp: string;
  let samsung: string;
  let lide: string;
  let keyboard: string;
  const hpScanner = () => `${hp} scanner`;
  const lideScanner = () => `${lide} scanner`;

  beforeAll(async () => {
    office = await startServer("office", "office");
    const devices = await fixtureDevices("office");
    const product = (vendorId: string) =>
      (devices.find((d) => d.vendorId === vendorId) as FixtureDevice).product;
    hp = product("03f0");
    samsung = product("04e8");
    lide = product("04a9");
    keyboard = product("046d");
  });
  afterAll(() => office?.stop());

  test("15. wizard: HP as printer and scanner, Samsung as printer only, LiDE as scanner; the keyboard is hidden", async () => {
    await scenario(office, desktop, async (s) => {
      const { page } = s;
      await s.goto("/");
      await page.waitForURL(/\/setup$/);
      await page.getByText(hp, { exact: true }).waitFor();
      await page.getByText(lide, { exact: true }).waitFor();
      // The keyboard can be neither, so it sits in the collapsed list of other devices.
      await page.getByRole("button", { name: setupStrings.detect.otherDevices(1) }).waitFor();
      expect(await page.getByText(keyboard, { exact: true }).isVisible()).toBe(false);
      const row = (name: string) =>
        page.locator(".setup-devices > li").filter({ has: page.getByText(name, { exact: true }) });
      const roleChoice = (name: string, role: "printer" | "scanner") =>
        row(name).getByRole("radiogroup", { name: setupStrings.detect.roleLabel[role] });
      // The LiDE has no printer interface, so it is offered as a scanner only.
      await roleChoice(lide, "scanner").waitFor();
      expect(await roleChoice(lide, "printer").count()).toBe(0);
      await roleChoice(samsung, "scanner").getByText(setupStrings.detect.dontUse).click();
      expect(
        await roleChoice(samsung, "scanner")
          .getByRole("radio", { name: setupStrings.detect.dontUse })
          .isChecked(),
      ).toBe(true);
      await s.inspect("office-setup-devices");
      await page.getByRole("button", { name: strings.buttons.next, exact: true }).click();

      await heading(page, setupStrings.steps.names).waitFor();
      for (const t of [
        setupStrings.names.printerTitle(hp),
        setupStrings.names.scannerTitle(hpScanner()),
        setupStrings.names.printerTitle(samsung),
        setupStrings.names.scannerTitle(lideScanner()),
      ])
        await page.getByText(t, { exact: true }).waitFor();
      expect(
        await page.getByText(setupStrings.names.scannerTitle(`${samsung} scanner`)).count(),
      ).toBe(0);
      await s.inspect("office-setup-names");
      await page.getByRole("button", { name: strings.buttons.back, exact: true }).click();
      await heading(page, setupStrings.steps.devices).waitFor();

      await runWizard(s, {
        prefix: "office-setup",
        inspectSteps: false,
        atDefaults: async () => {
          // The same control as Settings, from the live capabilities of all four entries.
          await card(page, samsung).waitFor();
          await card(page, lideScanner()).waitFor();
          await expectDisabledWithReason(page, card(page, samsung), title("sides"), p.oneSided);
          await expectDisabledWithReason(
            page,
            card(page, lideScanner()),
            title("source", "scanner"),
            p.noFeeder,
          );
          await expectSharedControl(page, title("media"));
          await expectSharedControl(page, title("source", "scanner"));
          await expectNoRawKeywords(page, "setup defaults");
        },
      });
      await page.getByRole("link", { name: setupStrings.done.goToStatus }).click();
      await heading(page, strings.nav.status).waitFor();
    });
    // Three devices ready: two printers (the Samsung on its legacy queue) and two scanners.
    const st = await waitForStatus(
      office,
      (x) =>
        x.printers.length === 2 &&
        x.scanners.length === 2 &&
        [...x.printers, ...x.scanners].every((e) => e.phase === "ready"),
    );
    expect(st.phase).toBe("ready");
    expect(st.printers.map((e) => e.name).sort()).toEqual([hp, samsung].sort());
    expect(st.scanners.map((e) => e.name).sort()).toEqual([hpScanner(), lideScanner()].sort());
    const cfg = (await (await fetch(`${office.url}/api/v1/config`)).json()) as {
      config: { ignoredDevices: { vendorId: string; roles: string[] }[] };
    };
    expect(cfg.config.ignoredDevices).toEqual([
      expect.objectContaining({ vendorId: "04e8", roles: ["scanner"] }),
    ]);
  });

  test("15. print: a picker of two printers, each with only the options it offers", async () => {
    await scenario(office, desktop, async (s) => {
      const { page } = s;
      await s.goto("/print");
      await heading(page, strings.nav.print).waitFor();
      const choices = page
        .getByRole("radiogroup", { name: devicePickerStrings.printer, exact: true })
        .getByRole("radio");
      await choices.nth(1).waitFor();
      expect(await choices.count()).toBe(2);

      // The Samsung prints one-sided in black and white only: no control, and no text for them.
      await pickerChoice(page, "printer", samsung).click();
      await trigger(page, title("media")).waitFor();
      await trigger(page, title("quality")).waitFor();
      expect(await field(page, title("colorMode")).count()).toBe(0);
      expect(await field(page, title("sides")).count()).toBe(0);
      // biome-ignore lint/performance/useDomNodeTextContent: the rendered text is what people see; hidden nodes must not count
      const lines = (await page.locator("main").innerText()).split("\n").map((l) => l.trim());
      for (const standIn of [
        title("colorMode"),
        title("sides"),
        optionLabel("sides", "one-sided", "printer"),
        optionLabel("colorMode", "monochrome", "printer"),
        p.oneSided,
      ])
        expect({ standIn, found: lines.some((l) => l.includes(standIn)) }).toEqual({
          standIn,
          found: false,
        });
      await expectSharedControl(page, title("media"));
      await expectNoRawKeywords(page, "print samsung");

      // The HP: trays and an envelope size; its single output tray is not a choice.
      await pickerChoice(page, "printer", hp).click();
      await trigger(page, title("mediaSource")).waitFor();
      expect(await optionsOf(page, title("mediaSource"))).toEqual(
        ["auto", "tray-1", "manual"].map((v) => optionLabel("mediaSource", v, "printer")),
      );
      expect(await optionsOf(page, title("media"))).toContain("Envelope #10 (4.125 × 9.5 in)");
      await page.getByRole("button", { name: printStrings.options.more, exact: true }).click();
      await trigger(page, title("orientation")).waitFor();
      expect(await field(page, title("outputBin")).count()).toBe(0);
      await s.inspect("office-print");
      await expectSharedControl(page, title("media"));
      await expectNoRawKeywords(page, "print hp");
    });
  });

  test("15. scan: a picker of two scanners; the LiDE has no source control", async () => {
    await scenario(office, desktop, async (s) => {
      const { page } = s;
      await s.goto("/scan");
      await heading(page, strings.nav.scan).waitFor();
      const choices = page
        .getByRole("radiogroup", { name: devicePickerStrings.scanner, exact: true })
        .getByRole("radio");
      await choices.nth(1).waitFor();
      expect(await choices.count()).toBe(2);

      await pickerChoice(page, "scanner", lideScanner()).click();
      await trigger(page, title("resolution", "scanner")).waitFor();
      expect(await field(page, title("source", "scanner")).count()).toBe(0);
      await expectNoRawKeywords(page, "scan lide");

      await pickerChoice(page, "scanner", hpScanner()).click();
      await trigger(page, title("source", "scanner")).waitFor();
      await s.inspect("office-scan");
      await expectSharedControl(page, title("source", "scanner"));
      await expectNoRawKeywords(page, "scan hp");
    });
  });

  test("15. settings: options a device cannot change are greyed out with the reason", async () => {
    await scenario(office, desktop, async (s) => {
      const { page } = s;
      await s.goto("/settings");
      await heading(page, strings.nav.settings).waitFor();
      for (const name of [hp, samsung, hpScanner(), lideScanner()])
        await entry(page, name)
          .getByRole("button", { name: settingsStrings.edit, exact: true })
          .click();

      await expectDisabledWithReason(page, entry(page, samsung), title("sides"), p.oneSided);
      await expectDisabledWithReason(
        page,
        entry(page, samsung),
        title("mediaSource"),
        p.notReported("printer"),
      );
      await expectDisabledWithReason(
        page,
        entry(page, hp),
        title("outputBin"),
        p.onlyOffers("printer", optionLabel("outputBin", "face-down", "printer")),
      );
      await expectDisabledWithReason(
        page,
        entry(page, lideScanner()),
        title("source", "scanner"),
        p.noFeeder,
      );
      await s.inspect("office-settings");
      await expectSharedControl(page, title("media"));
      await expectSharedControl(page, title("source", "scanner"));
      await expectNoRawKeywords(page, "settings");
    });
  });

  test("18. a reload keeps the remembered printer", async () => {
    const st = await apiStatus(office);
    const hpId = (st.printers.find((e) => e.name === hp) as StatusEntry).id;
    await scenario(office, desktop, async (s) => {
      const { page } = s;
      await s.goto("/print");
      await heading(page, strings.nav.print).waitFor();
      await pickerChoice(page, "printer", samsung).click();
      expect(await pickerChoice(page, "printer", samsung).isChecked()).toBe(true);
      await page.reload();
      await heading(page, strings.nav.print).waitFor();
      await pickerChoice(page, "printer", samsung).waitFor();
      await until(
        "the remembered printer",
        () => pickerChoice(page, "printer", samsung).isChecked(),
        (v) => v,
      );
      expect(await field(page, title("mediaSource")).count()).toBe(0);

      // A dashboard link names the printer, which is then remembered in turn.
      await s.goto(`/print?printer=${encodeURIComponent(hpId)}`);
      await trigger(page, title("mediaSource")).waitFor();
      expect(await pickerChoice(page, "printer", hp).isChecked()).toBe(true);
      await s.goto("/print");
      await trigger(page, title("mediaSource")).waitFor();
      expect(await pickerChoice(page, "printer", hp).isChecked()).toBe(true);
    });
  });

  test("19. multi-device Status: axe, overflow, screenshots and one rule under each collapsed disclosure", async () => {
    for (const vp of statusViewports) {
      await scenario(office, vp, async (s) => {
        const { page } = s;
        await s.goto("/");
        await heading(page, strings.nav.status).waitFor();
        const cards = page.locator(".status-device");
        await cards.nth(3).waitFor();
        expect(await cards.count()).toBe(4);
        await page.waitForTimeout(750);
        await s.inspect("status-office");
        // Every horizontal rule drawn just under each collapsed disclosure of a device card.
        // The disclosure's own bottom rule is the one; a second (the next section's top rule) is
        // the regression 1e774c4 fixed.
        const rules = await page.evaluate(() =>
          [...document.querySelectorAll(".status-device")].flatMap((c) =>
            [...c.querySelectorAll(".ui-disclosure:not([data-expanded])")].map((d) => {
              const edge = d.getBoundingClientRect().bottom;
              let n = 0;
              for (const el of c.querySelectorAll("*")) {
                const r = el.getBoundingClientRect();
                if (r.width === 0) continue;
                const cs = getComputedStyle(el);
                const drawn = (w: string, style: string, color: string) =>
                  Number.parseFloat(w) > 0 && style !== "none" && color !== "rgba(0, 0, 0, 0)";
                const near = (y: number) => y >= edge - 2 && y <= edge + 24;
                if (drawn(cs.borderTopWidth, cs.borderTopStyle, cs.borderTopColor) && near(r.top))
                  n++;
                if (
                  drawn(cs.borderBottomWidth, cs.borderBottomStyle, cs.borderBottomColor) &&
                  near(r.bottom)
                )
                  n++;
              }
              const name = c.querySelector(".ui-card-title")?.textContent ?? "";
              const what = d.querySelector("button")?.textContent ?? "";
              return `${name} / ${what}: ${n}`;
            }),
          ),
        );
        // Two collapsed disclosures (Connect a device, Technical details) per card.
        expect(rules.length).toBe(8);
        expect(rules.filter((r) => !r.endsWith(": 1"))).toEqual([]);
      });
    }
  });
});

// The CanoScan LiDE alone: a scanner and nothing to print with.
describe("standalone scanner", () => {
  let server: Server;
  let lide: string;
  beforeAll(async () => {
    server = await startServer("lide", "canon-lide-scanner");
    lide = await fixtureProduct("canon-lide-scanner");
  });
  afterAll(() => server?.stop());

  test("16. the wizard finds only a scanner, and Print is hidden", async () => {
    await scenario(server, desktop, async (s) => {
      const { page } = s;
      await s.goto("/");
      await page.waitForURL(/\/setup$/);
      const row = page
        .locator(".setup-devices > li")
        .filter({ has: page.getByText(lide, { exact: true }) });
      await row.getByRole("radiogroup", { name: setupStrings.detect.roleLabel.scanner }).waitFor();
      expect(
        await page.getByRole("radiogroup", { name: setupStrings.detect.roleLabel.printer }).count(),
      ).toBe(0);
      await runWizard(s, { prefix: "lide-setup", inspectSteps: false });
      await page.getByRole("link", { name: setupStrings.done.goToStatus }).click();
      await heading(page, strings.nav.status).waitFor();
      await nav(page, strings.nav.scan).waitFor();
      expect(await nav(page, strings.nav.print).count()).toBe(0);
      await s.goto("/print");
      await page.waitForURL((u) => u.pathname === "/");
      await heading(page, strings.nav.status).waitFor();
    });
    const st = await waitForStatus(server, (x) => x.phase === "ready");
    expect(st.printers).toEqual([]);
    expect(st.scanners.map((x) => x.name)).toEqual([`${lide} scanner`]);
  });

  test("17. disabling the only scanner in Settings hides Scan", async () => {
    await scenario(server, desktop, async (s) => {
      const { page } = s;
      await s.goto("/settings");
      await heading(page, strings.nav.settings).waitFor();
      await nav(page, strings.nav.scan).waitFor();
      const scanner = entry(page, `${lide} scanner`);
      await scanner.getByText(settingsStrings.fields.enabled, { exact: true }).click();
      expect(
        await scanner.getByRole("switch", { name: settingsStrings.fields.enabled }).isChecked(),
      ).toBe(false);
      await page.getByRole("button", { name: strings.buttons.save, exact: true }).click();
      await page.getByText(settingsStrings.saved).first().waitFor();
      await nav(page, strings.nav.scan).waitFor({ state: "detached" });
      expect(await nav(page, strings.nav.print).count()).toBe(0);
      await s.goto("/scan");
      await page.waitForURL((u) => u.pathname === "/");
      await heading(page, strings.nav.status).waitFor();
      await page.getByText(statusStrings.empty.title).waitFor();
      await s.inspect("status-nothing-in-use");
    });
    const cfg = await Bun.file(server.configPath).text();
    expect(cfg).toMatch(/enabled: false\n/);
  });
});
