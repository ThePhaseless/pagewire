import { afterEach, beforeEach, describe, expect, spyOn, test } from "bun:test";
import { act, cleanup, render, screen, waitFor, within } from "@testing-library/react";
import { createEventStore, EventsContext } from "../api/events";
import {
  type Job,
  type Scan,
  type Status as StatusDTO,
  type StatusPrinter,
  type StatusScanner,
  StatusSchema,
  type StatusWorkload,
  type USBDevice,
} from "../gen/api";
import { Status } from "./Status";

const realFetch = globalThis.fetch;
const json = (body: unknown) =>
  new Response(JSON.stringify(body), { headers: { "Content-Type": "application/json" } });

// GET /printers/{id}/jobs and GET /scans answer from these; never reach the network.
let jobsByPrinter: Record<string, Job[]> = {};
let scans: Scan[] = [];
let consoleError: ReturnType<typeof spyOn>;
beforeEach(() => {
  jobsByPrinter = {};
  scans = [];
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    const path = new URL(String(input), "http://localhost").pathname;
    const jobs = /^\/api\/v1\/printers\/([^/]+)\/jobs$/.exec(path);
    if (jobs) return json(jobsByPrinter[decodeURIComponent(jobs[1] ?? "")] ?? []);
    if (path === "/api/v1/scans") return json(scans);
    return new Response("not found", { status: 404 });
  }) as unknown as typeof fetch;
  consoleError = spyOn(console, "error");
});
afterEach(() => {
  // React reports duplicate keys and invalid markup, and the store dropped events, through console.error.
  expect(consoleError).not.toHaveBeenCalled();
  consoleError.mockRestore();
  cleanup();
  globalThis.fetch = realFetch;
});

const emptyCaps = {
  media: [],
  mediaSources: [],
  mediaTypes: [],
  sides: [],
  colorModes: [],
  qualities: [],
  contentOptimize: [],
  orientations: [],
  printScalings: [],
  outputBins: [],
  finishings: [],
  formats: [],
  resolutions: [],
  numberUp: [],
  copiesMax: 0,
  mediaDefault: "",
  mediaSourceDefault: "",
  mediaTypeDefault: "",
  sidesDefault: "",
  colorModeDefault: "",
  qualityDefault: "",
  contentOptimizeDefault: "",
  orientationDefault: "",
  printScalingDefault: "",
  outputBinDefault: "",
  finishingsDefault: "",
  resolutionDefault: 0,
  numberUpDefault: 0,
};

const hp: USBDevice = {
  vendorId: "03f0",
  productId: "2754",
  serial: "FAKE0005",
  manufacturer: "HP",
  product: "OfficeJet Pro",
  bus: 1,
  address: 4,
  interfaces: [{ class: 7, subClass: 1, protocol: 4 }],
  deviceId: "MFG:HP;MDL:OfficeJet Pro;",
};

const scx4300: USBDevice = {
  vendorId: "04e8",
  productId: "342e",
  serial: "FAKE0008",
  manufacturer: "Samsung Electronics Co., Ltd.",
  product: "SCX-4300 Series",
  bus: 1,
  address: 9,
  interfaces: [{ class: 7, subClass: 1, protocol: 2 }],
  deviceId: "MFG:Samsung;CMD:GDI;MDL:SCX-4300 Series;CLS:PRINTER;MODE:PCL;STATUS:IDLE;",
};

function printer(over: Partial<StatusPrinter> = {}): StatusPrinter {
  return {
    id: "hp",
    name: "HP OfficeJet Pro",
    location: "",
    enabled: true,
    phase: "ready",
    message: "",
    warnings: [],
    decision: {
      family: "ipp-usb",
      driver: "",
      driverDescription: "",
      requiredEulas: [],
      optionalEulas: [],
      rule: "ipp-over-usb",
      device: hp,
    },
    device: hp,
    available: true,
    uri: "ipp://192.168.1.20:60000/ipp/print",
    state: "idle",
    reasons: [],
    formats: [],
    convertedFormats: [],
    media: [],
    markers: [],
    capabilities: emptyCaps,
    connect: { dnssdName: "", ippUris: [] },
    ...over,
  };
}

function scanner(over: Partial<StatusScanner> = {}): StatusScanner {
  return {
    id: "hp-scanner",
    name: "HP OfficeJet Pro scanner",
    location: "",
    enabled: true,
    phase: "ready",
    message: "",
    warnings: [],
    decision: { family: "ipp-usb", saneBackends: [], rule: "ipp-over-usb", device: hp },
    device: hp,
    available: true,
    url: "http://192.168.1.20:60000/eSCL",
    state: "idle",
    capabilities: {
      sources: [],
      resolutions: [],
      colorModes: [],
      adf: false,
      duplex: false,
      intents: [],
    },
    connect: { dnssdName: "", esclUrls: [] },
    ...over,
  };
}

function workload(over: Partial<StatusWorkload> = {}): StatusWorkload {
  return {
    node: "",
    phase: "running",
    mode: "usb",
    message: "",
    advertised: [],
    blobs: [],
    ...over,
  };
}

function status(over: Partial<StatusDTO> = {}): StatusDTO {
  // Parsed like the event store does, so a fixture that drifts from the schema fails here.
  return StatusSchema.parse({
    config: {
      state: "applied",
      etag: "e1",
      appliedEtag: "e1",
      errors: [],
      changedAt: "0001-01-01T00:00:00Z",
    },
    phase: "ready",
    message: "",
    printers: [],
    scanners: [],
    workloads: [],
    devices: [],
    reconcile: [],
    ...over,
  });
}

class FakeEventSource {
  static last: FakeEventSource | null = null;
  onopen: ((ev: Event) => unknown) | null = null;
  onerror: ((ev: Event) => unknown) | null = null;
  listeners = new Map<string, ((e: MessageEvent) => void)[]>();
  constructor() {
    FakeEventSource.last = this;
  }
  addEventListener(type: string, fn: (e: MessageEvent) => void) {
    this.listeners.set(type, [...(this.listeners.get(type) ?? []), fn]);
  }
  close() {}
  emit(type: string, data: unknown) {
    for (const fn of this.listeners.get(type) ?? []) {
      fn(new MessageEvent(type, { data: JSON.stringify(data) }));
    }
  }
}

async function renderWith(s: StatusDTO | null) {
  const store = createEventStore({
    newEventSource: () => new FakeEventSource() as unknown as EventSource,
    notFoundAfterMs: 30,
  });
  store.start();
  if (s) act(() => FakeEventSource.last?.emit("status", s));
  // Async act lets the jobs and scans loaders settle inside act.
  await act(async () => {
    render(
      <EventsContext value={store}>
        <Status />
      </EventsContext>,
    );
  });
}

const emit = (type: string, data: unknown) => act(() => FakeEventSource.last?.emit(type, data));

/** The device card whose heading is `name`. */
const card = (name: string) =>
  screen.getByRole("heading", { level: 2, name }).closest("section") as HTMLElement;

describe("Status view", () => {
  test("renders before the first Status arrives", async () => {
    await renderWith(null);
    expect(screen.getByRole("heading", { level: 1, name: "Status" })).toBeTruthy();
  });

  test("with no enabled device it offers to detect and add devices", async () => {
    await renderWith(
      status({ phase: "none", printers: [printer({ enabled: false, phase: "none" })] }),
    );
    expect(screen.getByText("No printer or scanner in use")).toBeTruthy();
    expect(screen.getByRole("link", { name: "Detect and add devices" }).getAttribute("href")).toBe(
      "/setup?add=1",
    );
    // The disabled printer gets no card, and there are no jobs to show.
    expect(screen.queryByRole("heading", { name: "HP OfficeJet Pro" })).toBeNull();
    expect(screen.queryByRole("heading", { name: "Recent print jobs" })).toBeNull();
    expect(screen.getByRole("heading", { level: 2, name: "System" })).toBeTruthy();
  });

  test("shows one card per enabled printer and scanner, devices before the side cards", async () => {
    await renderWith(
      status({
        printers: [
          printer(),
          printer({ id: "scx", name: "Samsung SCX-4300", location: "Study", device: scx4300 }),
          printer({ id: "off", name: "Old printer", enabled: false, phase: "none" }),
        ],
        scanners: [scanner()],
      }),
    );
    const titles = screen.getAllByRole("heading", { level: 2 }).map((h) => h.textContent);
    expect(titles).toEqual([
      "HP OfficeJet Pro",
      "Samsung SCX-4300",
      "HP OfficeJet Pro scanner",
      "Recent print jobs",
      "Recent scans",
      "System",
    ]);
    expect(within(card("Samsung SCX-4300")).getByText("Study")).toBeTruthy();
    expect(
      within(card("HP OfficeJet Pro"))
        .getByRole("link", { name: "Print a document" })
        .getAttribute("href"),
    ).toBe("/print?printer=hp");
    expect(
      within(card("HP OfficeJet Pro scanner"))
        .getByRole("link", { name: "Scan a document" })
        .getAttribute("href"),
    ).toBe("/scan?scanner=hp-scanner");
    // A scanner card has no print action, and a printer card no scan action.
    expect(
      within(card("HP OfficeJet Pro scanner")).queryByRole("link", { name: "Print a document" }),
    ).toBeNull();
    expect(
      within(card("HP OfficeJet Pro")).queryByRole("link", { name: "Scan a document" }),
    ).toBeNull();
  });

  test("each card's badge follows its own phase, then what the device is doing", async () => {
    await renderWith(
      status({
        phase: "deploying",
        printers: [
          printer({ state: "processing" }),
          printer({
            id: "scx",
            name: "Samsung",
            phase: "deploying",
            available: false,
            state: "unknown",
            message: "Starting the printer application",
          }),
        ],
      }),
    );
    expect(within(card("HP OfficeJet Pro")).getByText("Printing")).toBeTruthy();
    expect(within(card("Samsung")).getByText("Starting")).toBeTruthy();
    expect(within(card("Samsung")).getByText("Starting the printer application")).toBeTruthy();
    expect(
      within(card("Samsung")).getByText("You can print here once the printer is running."),
    ).toBeTruthy();
  });

  test("a device in error shows its message as an alert in its own card", async () => {
    await renderWith(
      status({
        phase: "error",
        message: "queue failed",
        printers: [printer({ phase: "error", message: "queue failed", available: false })],
      }),
    );
    const alert = within(card("HP OfficeJet Pro")).getByRole("alert");
    expect(alert.textContent).toContain("queue failed");
    // The overall message repeats the card's, so it is not shown again above the cards.
    expect(screen.getAllByRole("alert").length).toBe(1);
  });

  test("workload errors appear once above the cards", async () => {
    await renderWith(
      status({
        phase: "error",
        message: "apply failed: forbidden",
        printers: [
          printer({ phase: "error", message: "The device workload failed", available: false }),
        ],
        scanners: [
          scanner({ phase: "error", message: "The device workload failed", available: false }),
        ],
        workloads: [workload({ phase: "failed", message: "Back-off pulling image" })],
      }),
    );
    const top = screen.getAllByRole("alert")[0] as HTMLElement;
    expect(top.textContent).toContain("A device workload has a problem");
    expect(top.textContent).toContain("Back-off pulling image");
    expect(top.textContent).toContain("apply failed: forbidden");
    expect(screen.getAllByText("Back-off pulling image").length).toBe(1);
  });

  test("a waiting workload's message appears once above the cards, without an alert", async () => {
    await renderWith(
      status({
        phase: "deploying",
        printers: [printer({ phase: "deploying", available: false, message: "Starting" })],
        workloads: [workload({ phase: "pending", message: "Not visible to the device plugin" })],
      }),
    );
    expect(screen.getAllByText("Not visible to the device plugin").length).toBe(1);
    expect(screen.queryByRole("alert")).toBeNull();
  });

  test("an overall error equal to a waiting workload's message is still shown", async () => {
    await renderWith(
      status({
        phase: "error",
        message: "apply failed",
        printers: [printer({ enabled: false, phase: "none", message: "apply failed" })],
        workloads: [workload({ phase: "running", message: "apply failed" })],
      }),
    );
    expect(screen.getByRole("alert").textContent).toContain("apply failed");
  });

  test("warnings are sentences with labels, never raw keywords", async () => {
    await renderWith(
      status({
        printers: [
          printer({
            warnings: [
              {
                option: "colorMode",
                value: "auto",
                using: "monochrome",
                message: "colorMode auto is not offered",
              },
              { option: "mediaSource", value: "tray-1", using: "", message: "mediaSource tray-1" },
              {
                option: "other",
                value: "",
                using: "",
                message: "Could not set the printer's defaults: timeout",
              },
            ],
          }),
        ],
        scanners: [
          scanner({
            warnings: [{ option: "intent", value: "photo", using: "", message: "intent photo" }],
          }),
        ],
      }),
    );
    const p = card("HP OfficeJet Pro");
    expect(
      within(p).getByText(
        "The default Colour “Automatic” is not offered by this printer; it uses “Black and white”.",
      ),
    ).toBeTruthy();
    expect(
      within(p).getByText("The default Paper tray “Tray 1” is not offered by this printer."),
    ).toBeTruthy();
    expect(within(p).getByText("Could not set the printer's defaults: timeout")).toBeTruthy();
    expect(p.textContent).not.toContain("monochrome");
    expect(p.textContent).not.toContain("tray-1");
    expect(
      within(card("HP OfficeJet Pro scanner")).getByText(
        "The default Optimise for “Photo” is not offered by this scanner.",
      ),
    ).toBeTruthy();
  });

  test("printer reasons, supplies and unnamed markers", async () => {
    await renderWith(
      status({
        printers: [
          printer({
            reasons: ["media-empty-error", "media-empty-warning", "none", "some-new-thing-report"],
            markers: [
              { name: "Black toner", color: "#000000", level: 42 },
              { name: "", color: "#00ffff", level: 10 },
              { name: "", color: "#ff00ff", level: 20 },
              { name: "Waste", color: "none", level: -2 },
            ],
          }),
        ],
      }),
    );
    expect(screen.getAllByText("Out of paper").length).toBe(1);
    expect(screen.getByText("Some new thing")).toBeTruthy();
    expect(screen.getByRole("meter", { name: "Black toner" })).toBeTruthy();
    expect(screen.getAllByRole("meter", { name: "Supply" }).length).toBe(2);
    expect(screen.getByText("level unknown")).toBeTruthy();
  });

  test("each card connects with its own addresses and advertised name", async () => {
    await renderWith(
      status({
        printers: [
          printer({
            connect: {
              dnssdName: "HP OfficeJet Pro",
              ippUris: ["ipp://192.168.1.20:60000/ipp/print"],
            },
          }),
          printer({ id: "scx", name: "Samsung", connect: { dnssdName: "", ippUris: [] } }),
        ],
        scanners: [
          scanner({
            connect: {
              dnssdName: "HP OfficeJet Pro",
              esclUrls: ["http://192.168.1.20:60000/eSCL"],
            },
          }),
        ],
      }),
    );
    const hpCard = card("HP OfficeJet Pro");
    expect(within(hpCard).getByRole("button", { name: "Connect a device" })).toBeTruthy();
    expect(within(hpCard).getByText("ipp://192.168.1.20:60000/ipp/print")).toBeTruthy();
    expect(within(hpCard).queryByText("http://192.168.1.20:60000/eSCL")).toBeNull();
    expect(within(hpCard).queryByText("Scanner address")).toBeNull();
    const sc = card("HP OfficeJet Pro scanner");
    expect(within(sc).getByText("http://192.168.1.20:60000/eSCL")).toBeTruthy();
    expect(within(sc).queryByText("Printer address")).toBeNull();
    const sam = card("Samsung");
    expect(within(sam).getByText("Not advertised yet")).toBeTruthy();
    expect(within(sam).getByText("Not available yet")).toBeTruthy();
  });

  test("technical details stay collapsed and name the driver, rule, node and services", async () => {
    await renderWith(
      status({
        printers: [
          printer({
            id: "scx",
            name: "Samsung",
            device: scx4300,
            decision: {
              family: "legacy",
              driver: "samsung--scx-4300--2-0-0-en",
              driverDescription: "",
              requiredEulas: [],
              optionalEulas: [],
              rule: "model",
              device: scx4300,
            },
            connect: { dnssdName: "Samsung", ippUris: [] },
          }),
        ],
        scanners: [
          scanner({
            id: "scx-scanner",
            name: "Samsung scanner",
            device: scx4300,
            decision: {
              family: "airsane",
              saneBackends: ["xerox_mfp"],
              rule: "sane-index",
              device: scx4300,
            },
          }),
        ],
        workloads: [
          workload({
            node: "n1",
            advertised: [
              { type: "_ipp._tcp", name: "Samsung", port: 8005 },
              { type: "_uscan._tcp", name: "Other", port: 8090 },
            ],
          }),
        ],
        devices: [
          {
            device: scx4300,
            node: "n1",
            canPrint: true,
            canScan: true,
            printerId: "scx",
            scannerId: "scx-scanner",
            ignored: [],
            suggestedPrinter: null,
            suggestedScanner: null,
            printerDecision: null,
            scannerDecision: null,
          },
        ],
      }),
    );
    const p = card("Samsung");
    const details = within(p).getByRole("button", { name: "Technical details" });
    expect(details.getAttribute("aria-expanded")).toBe("false");
    expect(within(p).getByText("Chosen by").closest("[hidden]")).toBeTruthy();
    act(() => details.click());
    expect(details.getAttribute("aria-expanded")).toBe("true");
    expect(within(p).getByText("Chosen by").closest("[hidden]")).toBeNull();
    expect(within(p).getByText("Samsung Electronics Co., Ltd. SCX-4300 Series")).toBeTruthy();
    expect(within(p).getByText("USB 04e8:342e, serial FAKE0008")).toBeTruthy();
    expect(within(p).getByText("n1")).toBeTruthy();
    expect(within(p).getByText("Classic CUPS drivers")).toBeTruthy();
    expect(within(p).getByText("samsung--scx-4300--2-0-0-en")).toBeTruthy();
    expect(within(p).getByText("Printer model")).toBeTruthy();
    // Only this printer's own service, not the whole workload's.
    expect(within(p).getByText("Printer: Samsung")).toBeTruthy();
    expect(within(p).queryByText(/Other/)).toBeNull();
    const s = card("Samsung scanner");
    expect(within(s).getByText("Listed in the SANE driver index")).toBeTruthy();
    expect(within(s).getByText("SANE")).toBeTruthy();
    expect(within(s).getByText("xerox_mfp")).toBeTruthy();
    expect(within(s).getByText(/Not discoverable on your network yet/)).toBeTruthy();
  });

  test("a family chosen in Settings without a driver name leaves the choice to the app", async () => {
    const d = printer().decision;
    if (!d) throw new Error("fixture");
    await renderWith(
      status({
        printers: [printer({ decision: { ...d, family: "gutenprint", rule: "override" } })],
      }),
    );
    expect(screen.getByText("Your settings")).toBeTruthy();
    expect(screen.getByText("Picked by the printer application")).toBeTruthy();
  });

  test("driverless printing names no driver", async () => {
    await renderWith(status({ printers: [printer()] }));
    const driver = screen.getByText("Driver").closest("div") as HTMLElement;
    expect(within(driver).getByText("None")).toBeTruthy();
    expect(screen.getByText("Driverless printing (IPP over USB)")).toBeTruthy();
  });

  test("a simulated printer has no USB facts", async () => {
    await renderWith(
      status({
        printers: [
          printer({
            id: "sim",
            name: "Living room printer",
            device: null,
            decision: {
              family: "sim",
              driver: "",
              driverDescription: "",
              requiredEulas: [],
              optionalEulas: [],
              rule: "simulated",
              device: null,
            },
          }),
        ],
        workloads: [workload({ mode: "simulated", blobs: [] })],
      }),
    );
    expect(screen.getByText("Simulated device")).toBeTruthy();
    expect(screen.getByText("Simulated mode")).toBeTruthy();
    expect(screen.queryByText("USB")).toBeNull();
  });

  test("no matching driver shows the device ID and a way to choose one in Settings", async () => {
    const noDriver = {
      family: "",
      driver: "",
      driverDescription: "",
      requiredEulas: [],
      optionalEulas: [],
      rule: "none" as const,
      device: scx4300,
    };
    await renderWith(
      status({
        printers: [
          printer({ id: "a", name: "A", phase: "degraded", device: scx4300, decision: noDriver }),
          printer({
            id: "b",
            name: "B",
            phase: "degraded",
            device: { ...scx4300, deviceId: "" },
            decision: { ...noDriver, device: { ...scx4300, deviceId: "" } },
          }),
        ],
      }),
    );
    const a = card("A");
    expect(within(a).getByText("Needs attention")).toBeTruthy();
    expect(within(a).getByText("No printer driver matches this printer")).toBeTruthy();
    expect(within(a).getByText(scx4300.deviceId).tagName).toBe("CODE");
    expect(
      within(a).getByRole("link", { name: "Choose a driver in Settings" }).getAttribute("href"),
    ).toBe("/settings");
    expect(within(a).getByText("No match")).toBeTruthy();
    expect(within(card("B")).getByText("The printer did not report a device ID.")).toBeTruthy();
  });

  test("vendor files show in the printer that needs them", async () => {
    const d = printer().decision;
    if (!d) throw new Error("fixture");
    await renderWith(
      status({
        printers: [
          printer({
            decision: { ...d, family: "hplip", rule: "model", optionalEulas: ["hplip-plugin"] },
          }),
          printer({ id: "other", name: "Other" }),
        ],
        workloads: [
          workload({
            blobs: [
              { id: "hplip-plugin", version: "3.24.4", state: "failed", message: "bad signature" },
            ],
          }),
        ],
      }),
    );
    const blob = within(card("HP OfficeJet Pro"))
      .getByText(/hplip-plugin 3\.24\.4/)
      .closest("li") as HTMLElement;
    expect(within(blob).getByText("Failed")).toBeTruthy();
    expect(within(blob).getByText("bad signature")).toBeTruthy();
    expect(within(card("Other")).queryByText(/hplip-plugin/)).toBeNull();
  });

  test("one rule under each collapsed disclosure in a card", async () => {
    await renderWith(
      status({
        printers: [
          printer({ markers: [{ name: "Black", color: "#000000", level: 50 }] }),
          printer({ id: "b", name: "B" }),
        ],
        scanners: [scanner()],
      }),
    );
    // happy-dom does not apply Status.css, so check the structure the CSS relies on: what follows a
    // disclosure is nothing, another disclosure (which draws no top rule), or a section whose top
    // rule the carried-over selector removes; and the first disclosure follows a non-disclosure.
    const disclosures = document.querySelectorAll(".status-device .ui-disclosure");
    expect(disclosures.length).toBe(6);
    for (const d of disclosures) {
      const next = d.nextElementSibling;
      expect(next === null || next.matches(".ui-disclosure, .status-section")).toBe(true);
    }
    const css = await Bun.file(new URL("./Status.css", import.meta.url)).text();
    expect(css).toMatch(/\.ui-disclosure \+ \.status-section \{\s*border-block-start: 0;/);
    expect(css).toMatch(
      /:not\(\.ui-disclosure\) \+ \.ui-disclosure \{\s*border-block-start: 1px solid var\(--rule\);/,
    );
  });

  test("the system card shows the settings file, its errors and open reconcile problems", async () => {
    await renderWith(
      status({
        config: {
          state: "invalid",
          etag: "e2",
          appliedEtag: "e1",
          errors: [
            { path: "/printers/0/mode", line: 4, column: 9, message: "must be usb or simulated" },
          ],
          changedAt: "2026-11-24T17:00:00.123+01:00",
        },
        printers: [printer()],
        reconcile: [
          {
            item: "workload/n1",
            ok: false,
            message: "apply failed: forbidden",
            at: "2026-11-24T17:00:00Z",
          },
          { item: "config-migration", ok: false, message: "read-only", at: "2026-11-24T17:00:00Z" },
          { item: "data-dir", ok: true, message: "", at: "2026-11-24T17:00:00Z" },
        ],
      }),
    );
    const sys = card("System");
    expect(within(sys).getByText("Has errors")).toBeTruthy();
    expect(within(sys).getByText("Line 4, column 9: must be usb or simulated")).toBeTruthy();
    expect(within(sys).getByText(/^Changed /)).toBeTruthy();
    expect(within(sys).getByText("Devices on n1")).toBeTruthy();
    expect(within(sys).getByText("apply failed: forbidden")).toBeTruthy();
    expect(within(sys).getByText("Settings file update")).toBeTruthy();
    expect(within(sys).queryByText("Data directory")).toBeNull();
    expect(within(sys).getByRole("link", { name: "Open Settings" }).getAttribute("href")).toBe(
      "/settings",
    );
  });

  test("the zero changedAt is not a date", async () => {
    await renderWith(
      status({
        config: {
          state: "missing",
          etag: "",
          appliedEtag: "",
          errors: [],
          changedAt: "0001-01-01T00:00:00Z",
        },
      }),
    );
    expect(screen.getByText("No settings file")).toBeTruthy();
    expect(screen.queryByText(/^Changed /)).toBeNull();
  });

  test("a job row moves from pending to completed after a job event", async () => {
    const job: Job = {
      id: 7,
      printer: "hp",
      name: "boarding-pass.pdf",
      state: "pending",
      reasons: [],
      createdAt: "2026-11-24T17:00:00+01:00",
    };
    jobsByPrinter = { hp: [job] };
    await renderWith(status({ printers: [printer()] }));
    const row = await waitFor(
      () => screen.getByText("boarding-pass.pdf").closest("li") as HTMLElement,
    );
    expect(within(row).getByText("Waiting")).toBeTruthy();
    emit("job", { ...job, state: "completed" });
    await waitFor(() =>
      expect(
        within(screen.getByText("boarding-pass.pdf").closest("li") as HTMLElement).getByText(
          "Sent to printer",
        ),
      ).toBeTruthy(),
    );
    expect(screen.getAllByText("boarding-pass.pdf").length).toBe(1);
  });

  test("a printing job uses the active tone, a waiting one busy", async () => {
    const at = "2026-11-24T17:00:00+01:00";
    jobsByPrinter = {
      hp: [
        { id: 1, printer: "hp", name: "a.pdf", state: "pending", reasons: [], createdAt: at },
        { id: 2, printer: "hp", name: "b.pdf", state: "processing", reasons: [], createdAt: at },
      ],
    };
    await renderWith(status({ printers: [printer()] }));
    const tone = (name: string) =>
      (screen.getByText(name).closest("li") as HTMLElement)
        .querySelector(".ui-badge")
        ?.getAttribute("data-tone");
    await waitFor(() => expect(tone("a.pdf")).toBe("busy"));
    expect(tone("b.pdf")).toBe("active");
  });

  test("with several printers each lists its own jobs under its name", async () => {
    const at = "2026-11-24T17:00:00+01:00";
    jobsByPrinter = {
      hp: [
        { id: 1, printer: "hp", name: "hp.pdf", state: "completed", reasons: [], createdAt: at },
      ],
    };
    await renderWith(status({ printers: [printer(), printer({ id: "scx", name: "Samsung" })] }));
    const jobs = card("Recent print jobs");
    await waitFor(() => expect(within(jobs).getByText("hp.pdf")).toBeTruthy());
    const hpJobs = within(jobs)
      .getByRole("heading", { level: 3, name: "HP OfficeJet Pro" })
      .closest("section") as HTMLElement;
    expect(
      within(hpJobs)
        .getByRole("link", { name: "All jobs on HP OfficeJet Pro" })
        .getAttribute("href"),
    ).toBe("/print?printer=hp");
    const samJobs = within(jobs)
      .getByRole("heading", { level: 3, name: "Samsung" })
      .closest("section") as HTMLElement;
    expect(within(samJobs).getByText("No print jobs yet")).toBeTruthy();
  });

  test("recent scans name their scanner and follow scan events", async () => {
    const scan: Scan = {
      id: "s1",
      scanner: "hp-scanner",
      state: "scanning",
      createdAt: "2026-11-24T17:00:00+01:00",
      pages: 0,
      format: "pdf",
      settings: {
        scanner: "hp-scanner",
        source: "platen",
        resolution: 300,
        colorMode: "color",
        format: "pdf",
      },
      error: null,
    };
    scans = [scan];
    await renderWith(status({ scanners: [scanner()] }));
    const list = card("Recent scans");
    const row = await waitFor(
      () => within(list).getByText("HP OfficeJet Pro scanner").closest("li") as HTMLElement,
    );
    expect(within(row).getByText("Scanning")).toBeTruthy();
    emit("scan", { ...scan, state: "completed", pages: 2 });
    await waitFor(() => expect(within(list).getByText("Done")).toBeTruthy());
    expect(within(list).getByText("2 pages")).toBeTruthy();
    expect(within(list).getAllByRole("listitem").length).toBe(1);
  });
});
