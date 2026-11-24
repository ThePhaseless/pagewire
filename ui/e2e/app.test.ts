// Browser end-to-end scenarios against the real binary in sim mode, at three viewports.
// Every scenario runs under the harness's console and API status rules.
import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { join } from "node:path";
import type { Page } from "playwright-core";
import { eulaStrings } from "../src/components/EulaAccept.strings";
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

const samplePdf = join(repoRoot, "internal/printing/testdata/sample.pdf");
const fixtureProduct = async (name: string): Promise<string> => {
  const f = await Bun.file(join(repoRoot, `internal/catalog/fixtures/${name}.json`)).json();
  return f.devices[0].product;
};

type StatusJSON = {
  device: { phase: string; mode: string; decision: { device: unknown } | null };
  config: { state: string };
};

async function apiStatus(server: Server): Promise<StatusJSON> {
  return (await fetch(`${server.url}/api/v1/status`)).json() as Promise<StatusJSON>;
}

/** Polls Status every second until `ok` holds; reports the last Status on timeout. */
async function waitForStatus(server: Server, ok: (s: StatusJSON) => boolean, timeoutMs = 120_000) {
  const deadline = Date.now() + timeoutMs;
  let last: StatusJSON | undefined;
  while (Date.now() < deadline) {
    last = await apiStatus(server);
    if (ok(last)) return last;
    await Bun.sleep(1000);
  }
  throw new Error(`Status did not reach the expected state: ${JSON.stringify(last?.device)}`);
}

/** Creates the config from setup.suggested through the API, skipping the wizard. */
async function seedConfig(server: Server) {
  const setup = (await (await fetch(`${server.url}/api/v1/setup`)).json()) as {
    suggested: unknown;
  };
  const r = await fetch(`${server.url}/api/v1/config`, {
    method: "PUT",
    headers: { "Content-Type": "application/json", "If-None-Match": "*" },
    body: JSON.stringify(setup.suggested),
  });
  if (r.status !== 201) throw new Error(`seeding the config: ${r.status} ${await r.text()}`);
  await waitForStatus(server, (s) => s.device.phase === "ready");
}

const nav = (page: Page, label: string) =>
  page.getByRole("navigation", { name: strings.nav.label }).getByRole("link", { name: label });

const heading = (page: Page, name: string) =>
  page.getByRole("heading", { level: 1, name, exact: true });

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
async function tabTo(page: Page, name: string | RegExp, max = 60) {
  const seen: string[] = [];
  for (let i = 0; i < max; i++) {
    await page.keyboard.press("Tab");
    const got = await focusedName(page);
    if (typeof name === "string" ? got === name : name.test(got)) return;
    seen.push(got);
  }
  throw new Error(`Tab never reached ${name}; focus went through: ${seen.join(" | ")}`);
}

type Step = "detect" | "name" | "driver" | "defaults";

/** Pointer path through the wizard to the done screen, checking each step on the way. */
async function runWizard(s: Session, steps: Step[] = ["detect", "name", "driver", "defaults"]) {
  const { page } = s;
  const next = page.getByRole("button", { name: strings.buttons.next, exact: true });
  for (const step of steps) {
    await heading(page, setupStrings.steps[step]).waitFor();
    await s.inspect(`setup-${step}`);
    await next.click();
  }
  await heading(page, setupStrings.steps.review).waitFor();
  await s.inspect("setup-review");
  await page.getByRole("button", { name: setupStrings.review.finish }).click();
  await heading(page, setupStrings.done.title).waitFor({ timeout: 180_000 });
  await s.inspect("setup-done");
}

/** Picks an option in a react-aria Select by its label. */
async function choose(page: Page, label: string, option: string) {
  await page.getByRole("button", { name: new RegExp(label) }).click();
  await page.getByRole("option", { name: option, exact: true }).click();
}

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
      expect(st.device.phase).toBe("ready");
      expect(st.device.mode).toBe("usb");
    });

    test(`2. Status shows connect URIs (${vp.name})`, async () => {
      await scenario(main, vp, async (s) => {
        await s.goto("/");
        await heading(s.page, strings.nav.status).waitFor();
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
        await page.getByRole("button", { name: printStrings.submit(1, 1), exact: true }).click();
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

    test(`4. scan: two pages, preview and PDF download (${vp.name})`, async () => {
      await scenario(main, vp, async (s) => {
        const { page } = s;
        await s.goto("/");
        await heading(page, strings.nav.status).waitFor();
        await nav(page, strings.nav.scan).click();
        await heading(page, strings.nav.scan).waitFor();
        await choose(page, scanStrings.options.source.label, scanStrings.options.source.adf);
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
      const location = `Office ${vp.width}`;
      const comment = "# edited on disk by the e2e test";
      await scenario(main, vp, async (s) => {
        const { page } = s;
        // The file changes under the open form, so the save gets a 412 by design.
        s.expectApiStatus("PATCH", "/api/v1/config", 412);
        await s.goto("/settings");
        const field = page.getByRole("textbox", { name: settingsStrings.fields.location });
        await field.fill(location);

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
        await page
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
          await s.goto("/setup");
          await page.getByText(product).waitFor();
          await tabTo(page, strings.buttons.next);
          await page.keyboard.press("Enter");
          await heading(page, setupStrings.steps.name).waitFor();
          await tabTo(page, new RegExp(`^${setupStrings.name.nameLabel}`));
          await page.keyboard.press("ControlOrMeta+a");
          await page.keyboard.type("Keyboard printer");
          for (const step of ["driver", "defaults", "review"] as const) {
            await tabTo(page, strings.buttons.next);
            await page.keyboard.press("Enter");
            await heading(page, setupStrings.steps[step]).waitFor();
          }
          await tabTo(page, setupStrings.review.finish);
          await page.keyboard.press("Enter");
          await heading(page, setupStrings.done.title).waitFor({ timeout: 180_000 });
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
        await seedConfig(server);
        const setup = (await (await fetch(`${server.url}/api/v1/setup`)).json()) as {
          eulas: { id: string; title: string }[];
        };
        const eula = setup.eulas.find((e) => e.id === "hplip-plugin");
        if (!eula) throw new Error(`setup lists no hplip-plugin EULA: ${JSON.stringify(setup)}`);
        await scenario(server, vp, async (s) => {
          const { page } = s;
          // Accepting adds the blob, which rolls the device; jobs are 503 until the printer is back.
          s.expectApiStatus("GET", "/api/v1/jobs", 503);
          await s.goto("/settings");
          await page.getByText(eulaStrings.accept(eula.title)).click();
          await page.getByRole("button", { name: strings.buttons.save, exact: true }).click();
          await page.getByText(settingsStrings.saved).first().waitFor();
          await nav(page, strings.nav.status).click();
          await heading(page, strings.nav.status).waitFor();
          await page.getByText(statusStrings.device.blobsTitle).waitFor({ timeout: 90_000 });
          await page
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
          // Choosing the simulated printer moves straight on to the name step.
          await runWizard(s, ["name", "driver", "defaults"]);
          await page.getByRole("link", { name: setupStrings.done.goToStatus }).click();
          await heading(page, strings.nav.status).waitFor();
          await page.getByText(statusStrings.device.simulatedDevice).waitFor();
          await page
            .getByText(statusStrings.device.mode.simulated, { exact: true })
            .first()
            .waitFor();
          expect(await page.getByText(strings.problems["bad-response"]).count()).toBe(0);
          await s.inspect("status-simulated");
        });
        const st = await apiStatus(server);
        expect(st.device.mode).toBe("simulated");
        expect(st.device.decision?.device).toBeNull();
      } finally {
        await server.stop();
      }
    });
  });
}

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

      // An API 404 makes Chromium log "Failed to load resource" (A15). That line is excluded from
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
