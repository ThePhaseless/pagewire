import { afterEach, beforeEach, describe, expect, mock, spyOn, test } from "bun:test";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { createEventStore, type EventStore, EventsContext } from "../api/events";
import { EulaAccept } from "../components/EulaAccept";
import type {
  Config,
  ConfigResponse,
  DetectedDevice,
  EULA,
  Printer,
  PrinterCaps,
  PrinterDecision,
  Scanner,
  ScannerCaps,
  ScannerDecision,
  Setup as SetupDTO,
  SetupEULA,
  Status,
  StatusPrinter,
  StatusScanner,
  USBDevice,
} from "../gen/api";
import { mediaForLanguage, Setup } from "./Setup";

// The Status the controller returns before any input: no config, no device, nothing reconciled.
const initialStatus: Status = {
  config: {
    state: "missing",
    etag: "",
    appliedEtag: "",
    errors: [],
    changedAt: "0001-01-01T00:00:00Z",
  },
  phase: "none",
  message: "",
  printers: [],
  scanners: [],
  workloads: [],
  devices: [],
  reconcile: [],
};

const hpDevice: USBDevice = {
  vendorId: "03f0",
  productId: "042a",
  serial: "FAKE0001",
  manufacturer: "Hewlett-Packard",
  product: "HP LaserJet Professional M1132 MFP",
  bus: 1,
  address: 5,
  interfaces: [
    { class: 7, subClass: 1, protocol: 2 },
    { class: 255, subClass: 255, protocol: 255 },
  ],
  deviceId: "MFG:Hewlett-Packard;MDL:HP LaserJet Professional M1132 MFP;CMD:ZJS,PJL;",
};
const scxDevice: USBDevice = {
  vendorId: "04e8",
  productId: "342e",
  serial: "SCX0001",
  manufacturer: "Samsung Electronics Co., Ltd.",
  product: "SCX-4300 Series",
  bus: 1,
  address: 9,
  interfaces: [
    { class: 255, subClass: 255, protocol: 255 },
    { class: 7, subClass: 1, protocol: 2 },
  ],
  deviceId: "MFG:Samsung;MDL:SCX-4300 Series;CMD:SPL;",
};
const keyboard: USBDevice = {
  vendorId: "046d",
  productId: "c31c",
  serial: "",
  manufacturer: "Logitech",
  product: "USB Keyboard",
  bus: 1,
  address: 2,
  interfaces: [{ class: 3, subClass: 1, protocol: 1 }],
  deviceId: "",
};

const hpDecision: PrinterDecision = {
  family: "hplip",
  driver: "hp--laserjet-professional-m-1132-mfp--requires-proprietary-plugin-en",
  driverDescription: "HP LaserJet Professional M1132 MFP, hpcups (en)",
  requiredEulas: [],
  optionalEulas: ["hplip-plugin"],
  rule: "model",
  device: hpDevice,
};
const scannerDecision = (device: USBDevice): ScannerDecision => ({
  family: "airsane",
  saneBackends: ["hpaio"],
  rule: "sane-index",
  device,
});

const printerEntry = (id: string, name: string, dev: USBDevice): Printer => ({
  id,
  name,
  location: "",
  enabled: true,
  mode: "usb",
  usb: { vendorId: dev.vendorId, productId: dev.productId, serial: dev.serial },
  driver: "auto",
  defaults: {},
});
const scannerEntry = (id: string, name: string, dev: USBDevice): Scanner => ({
  id,
  name,
  location: "",
  enabled: true,
  mode: "usb",
  usb: { vendorId: dev.vendorId, productId: dev.productId, serial: dev.serial },
  backend: "auto",
  defaults: {},
});

/** A detected device with both roles suggested (an MFP), unless `over` says otherwise. */
function detected(dev: USBDevice, id: string, over: Partial<DetectedDevice> = {}): DetectedDevice {
  return {
    device: dev,
    node: "",
    canPrint: true,
    canScan: true,
    printerId: "",
    scannerId: "",
    ignored: [],
    suggestedPrinter: printerEntry(id, dev.product, dev),
    suggestedScanner: scannerEntry(`${id}-scanner`, `${dev.product} scanner`, dev),
    printerDecision:
      id === "hp"
        ? hpDecision
        : { ...hpDecision, family: "legacy", optionalEulas: [], device: dev },
    scannerDecision: scannerDecision(dev),
    ...over,
  };
}
const hp = detected(hpDevice, "hp");
const scx = detected(scxDevice, "scx-4300");
const kbd = detected(keyboard, "kbd", {
  canPrint: false,
  canScan: false,
  suggestedPrinter: null,
  suggestedScanner: null,
  printerDecision: null,
  scannerDecision: null,
});

const SHA_A = "a".repeat(64);
const SHA_B = "b".repeat(64);
const LICENCE = "HP PLUG-IN LICENSE AGREEMENT\nRead this carefully.";
// What GET /eulas/hplip-plugin answers; tests replace it to walk pending, failed and changed texts.
let eulaResponses: (EULA | Promise<EULA>)[] = [];
const eulaOf = (over: Partial<EULA> = {}): EULA => ({
  id: "hplip-plugin",
  version: "3.24.4",
  title: "HP Linux Imaging and Printing proprietary plug-in",
  vendor: "HP",
  summary: "Needed by some HP lasers for firmware upload and by some HP scanners.",
  state: "available",
  message: "",
  textSha256: SHA_A,
  text: LICENCE,
  ...over,
});
const hplipEula: SetupEULA = {
  id: "hplip-plugin",
  version: "3.24.4",
  title: "HP Linux Imaging and Printing proprietary plug-in",
  vendor: "HP",
  summary: "Needed by some HP lasers for firmware upload and by some HP scanners.",
  required: false,
  state: "available",
  textSha256: SHA_A,
};

function emptyConfig(name: string): Config {
  return {
    apiVersion: "pagewire/v2",
    name,
    printers: [],
    scanners: [],
    ignoredDevices: [],
    print: { maxUploadMiB: 50 },
    scan: { retentionDays: 7 },
    licenses: { acceptedEulas: [] },
  };
}

const setupOf = (devices: DetectedDevice[], over: Partial<SetupDTO> = {}): SetupDTO => ({
  required: true,
  backend: "k8s",
  usbResourceConfigured: true,
  devices,
  eulas: [hplipEula],
  suggested: emptyConfig("Printer"),
  ...over,
});
const hpSetup = setupOf([hp]);
const noUsbSetup = setupOf([], { backend: "sim", usbResourceConfigured: false, eulas: [] });

const printerCaps = (c: Partial<PrinterCaps>): PrinterCaps => ({
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
  formats: ["application/pdf"],
  resolutions: [],
  numberUp: [],
  copiesMax: 99,
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
  ...c,
});
// The Samsung's live attributes: one-sided, monochrome, three qualities, 300/600 dpi, Letter.
const samsungCaps = printerCaps({
  media: ["na_letter_8.5x11in", "iso_a4_210x297mm"],
  mediaDefault: "na_letter_8.5x11in",
  sides: ["one-sided"],
  sidesDefault: "one-sided",
  colorModes: ["monochrome"],
  colorModeDefault: "monochrome",
  qualities: ["draft", "normal", "high"],
  qualityDefault: "normal",
  resolutions: [300, 600],
  resolutionDefault: 600,
});
const hpCaps = printerCaps({
  media: ["na_letter_8.5x11in", "iso_a4_210x297mm"],
  mediaDefault: "na_letter_8.5x11in",
  sides: ["one-sided", "two-sided-long-edge"],
  sidesDefault: "one-sided",
  colorModes: ["monochrome"],
  qualities: ["draft", "normal", "high"],
  qualityDefault: "normal",
});
const scanCaps: ScannerCaps = {
  sources: ["platen", "adf"],
  resolutions: [150, 300, 600],
  colorModes: ["color", "grayscale"],
  adf: true,
  duplex: false,
  intents: [],
};

function statusPrinter(
  p: Printer,
  caps: PrinterCaps,
  over: Partial<StatusPrinter> = {},
): StatusPrinter {
  return {
    id: p.id,
    name: p.name,
    location: p.location,
    enabled: true,
    phase: "ready",
    message: "",
    warnings: [],
    decision: null,
    device: null,
    available: true,
    uri: "",
    state: "idle",
    reasons: [],
    formats: [],
    convertedFormats: [],
    media: [],
    markers: [],
    capabilities: caps,
    connect: { dnssdName: "", ippUris: [] },
    ...over,
  };
}
function statusScanner(sc: Scanner, over: Partial<StatusScanner> = {}): StatusScanner {
  return {
    id: sc.id,
    name: sc.name,
    location: sc.location,
    enabled: true,
    phase: "ready",
    message: "",
    warnings: [],
    decision: null,
    device: null,
    available: true,
    url: "",
    state: "idle",
    capabilities: scanCaps,
    connect: { dnssdName: "", esclUrls: [] },
    ...over,
  };
}
const applied = (over: Partial<Status>): Status => ({
  ...initialStatus,
  config: { ...initialStatus.config, state: "applied", etag: "e1", appliedEtag: "e1" },
  ...over,
});

class FakeEventSource {
  static last: FakeEventSource;
  onopen: ((ev: Event) => unknown) | null = null;
  onerror: ((ev: Event) => unknown) | null = null;
  listeners = new Map<string, (e: MessageEvent) => void>();
  constructor() {
    FakeEventSource.last = this;
  }
  addEventListener(type: string, fn: (e: MessageEvent) => void) {
    this.listeners.set(type, fn);
  }
  close() {}
  status(s: Status) {
    act(() => {
      this.listeners.get("status")?.(new MessageEvent("status", { data: JSON.stringify(s) }));
    });
  }
}

type Call = { method: string; url: string; headers: Record<string, string>; body: unknown };
let calls: Call[] = [];
let putResponse: () => Response;
let patchResponse: () => Response;
// What GET /config answers, in order; the last one repeats.
let configResponses: ConfigResponse[] = [];
const realFetch = globalThis.fetch;
const json = (body: unknown, status = 200) =>
  new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
const configResponse = (config: Config, etag: string): ConfigResponse => ({
  etag,
  config,
  state: "applied",
  errors: [],
  lastGood: null,
});

function mockServer(setup: () => SetupDTO) {
  calls = [];
  globalThis.fetch = (async (input: RequestInfo | URL, init: RequestInit = {}) => {
    const url = String(input);
    const method = init.method ?? "GET";
    calls.push({
      method,
      url,
      headers: (init.headers ?? {}) as Record<string, string>,
      body: init.body ? JSON.parse(String(init.body)) : undefined,
    });
    if (url.endsWith("/api/v1/setup")) return json(setup());
    if (url.endsWith("/api/v1/setup/detect") && method === "POST")
      return new Response(null, { status: 202 });
    if (url.endsWith("/api/v1/eulas/hplip-plugin"))
      return json(
        await (eulaResponses.length > 1 ? eulaResponses.shift() : (eulaResponses[0] ?? eulaOf())),
      );
    if (url.endsWith("/api/v1/config") && method === "PUT") return putResponse();
    if (url.endsWith("/api/v1/config") && method === "PATCH") return patchResponse();
    if (url.endsWith("/api/v1/config") && method === "GET" && configResponses.length > 0)
      return json(configResponses.length > 1 ? configResponses.shift() : configResponses[0]);
    return json(
      { type: "about:blank", title: "Not found", status: 404, detail: "", errors: [] },
      404,
    );
  }) as unknown as typeof fetch;
}

let store: EventStore;
function mount(props: Parameters<typeof Setup>[0] = {}) {
  store = createEventStore({
    newEventSource: () => new FakeEventSource() as unknown as EventSource,
  });
  store.start();
  return render(
    <EventsContext value={store}>
      <Setup {...props} />
    </EventsContext>,
  );
}

const press = (name: string | RegExp, root: HTMLElement = document.body) => {
  const el = within(root).getByRole("button", { name });
  fireEvent.keyDown(el, { key: "Enter" });
  fireEvent.keyUp(el, { key: "Enter" });
};
const heading = (name: string | RegExp) => screen.findByRole("heading", { level: 1, name });
const deviceItem = (product: string) =>
  screen.getByText(product, { selector: ".setup-device-name" }).closest("li") as HTMLElement;
/** Chooses "Don't use" for one role of the device listed as `product`. */
const decline = (product: string, role: "printer" | "scanner") => {
  const group = within(deviceItem(product)).getByRole("radiogroup", {
    name: role === "printer" ? "As a printer" : "As a scanner",
  });
  fireEvent.click(within(group).getByRole("radio", { name: "Don't use" }));
};
const lastCall = (method: string) => {
  const c = calls.filter((x) => x.method === method).at(-1);
  if (!c) throw new Error(`no ${method} request`);
  return c;
};
/** The accessible description of a Select trigger, through aria-describedby. */
const description = (trigger: HTMLElement) =>
  (trigger.getAttribute("aria-describedby") ?? "")
    .split(" ")
    .filter(Boolean)
    .map((id) => document.getElementById(id)?.textContent ?? "")
    .join(" ");
async function choose(trigger: RegExp, option: string, root: HTMLElement = document.body) {
  fireEvent.click(within(root).getByRole("button", { name: trigger }));
  fireEvent.click(await screen.findByRole("option", { name: option }));
  await waitFor(() =>
    expect(within(root).getByRole("button", { name: trigger }).textContent).toContain(option),
  );
}

beforeEach(() => {
  eulaResponses = [];
  configResponses = [];
  window.history.replaceState(null, "", "/setup");
  putResponse = () => json(configResponse(lastCall("PUT").body as Config, "e1"), 201);
  patchResponse = () => json(configResponse(emptyConfig("x"), "e2"));
});
afterEach(() => {
  cleanup();
  store?.stop();
  globalThis.fetch = realFetch;
  window.history.replaceState(null, "", "/");
});

describe("mediaForLanguage", () => {
  test("Letter for the US and Canada, A4 elsewhere", () => {
    expect(mediaForLanguage("en-US")).toBe("na_letter_8.5x11in");
    expect(mediaForLanguage("en-ca")).toBe("na_letter_8.5x11in");
    expect(mediaForLanguage("en-GB")).toBe("iso_a4_210x297mm");
    expect(mediaForLanguage("pl")).toBe("iso_a4_210x297mm");
  });
});

describe("EulaAccept", () => {
  const renderEula = (accepted: string | undefined, onChange = mock((_: string | null) => {})) => {
    mockServer(() => hpSetup);
    render(<EulaAccept eula={hplipEula} accepted={accepted} onChange={onChange} pollMs={5} />);
    return onChange;
  };

  test("shows the exact text in a focusable region and accepts it with its sha256", async () => {
    const onChange = renderEula(undefined);
    const region = await screen.findByRole("region", {
      name: "HP Linux Imaging and Printing proprietary plug-in licence text",
    });
    expect(region.textContent).toBe(LICENCE);
    expect(region.getAttribute("tabindex")).toBe("0");
    const box = screen.getByRole("checkbox", {
      name: "I accept the HP Linux Imaging and Printing proprietary plug-in licence shown above, version 3.24.4",
    }) as HTMLInputElement;
    expect(box.checked).toBe(false);
    expect(document.body.textContent).toContain("HP, version 3.24.4");
    expect(document.body.textContent).toContain("only after you accept");
    fireEvent.click(box);
    expect(onChange).toHaveBeenCalledWith(SHA_A);
  });

  test("pending shows progress and asks again until the text is available", async () => {
    // The second answer waits until the pending state has been checked.
    let release!: () => void;
    const second = new Promise<EULA>((r) => {
      release = () => r(eulaOf());
    });
    eulaResponses = [eulaOf({ state: "pending", text: null, textSha256: null }), second];
    renderEula(undefined);
    expect(await screen.findByRole("progressbar")).toBeTruthy();
    expect((screen.getByRole("checkbox") as HTMLInputElement).disabled).toBe(true);
    release();
    await screen.findByRole("region", { name: /licence text/ });
    expect((screen.getByRole("checkbox") as HTMLInputElement).disabled).toBe(false);
    expect(calls.filter((c) => c.url.endsWith("/eulas/hplip-plugin")).length).toBe(2);
  });

  test("failed shows the agent's message and cannot be accepted", async () => {
    eulaResponses = [
      eulaOf({
        state: "failed",
        message: "download: no route to host",
        text: null,
        textSha256: null,
      }),
    ];
    renderEula(undefined);
    await screen.findByText("download: no route to host");
    expect(document.body.textContent).toContain("cannot install HP Linux Imaging");
    expect((screen.getByRole("checkbox") as HTMLInputElement).disabled).toBe(true);
    // Try again asks once more; this time the agent has the text.
    eulaResponses = [eulaOf()];
    fireEvent.click(screen.getByRole("button", { name: "Try again" }));
    await screen.findByRole("region", { name: /licence text/ });
  });

  test("after a failure it asks again on its own and shows the text once the agent has it", async () => {
    let release!: () => void;
    const second = new Promise<EULA>((r) => {
      release = () => r(eulaOf());
    });
    eulaResponses = [
      eulaOf({ state: "failed", message: "download: timeout", text: null, textSha256: null }),
      second,
    ];
    mockServer(() => hpSetup);
    render(
      <EulaAccept eula={hplipEula} accepted={undefined} onChange={() => {}} failedPollMs={20} />,
    );
    await screen.findByText("download: timeout");
    release();
    await screen.findByRole("region", { name: /licence text/ });
    expect(calls.filter((c) => c.url.endsWith("/eulas/hplip-plugin")).length).toBe(2);
    expect((screen.getByRole("checkbox") as HTMLInputElement).disabled).toBe(false);
  });

  test("a changed text asks for acceptance again and records the new sha256", async () => {
    eulaResponses = [eulaOf({ textSha256: SHA_B, text: "Version two." })];
    const onChange = renderEula(SHA_A);
    await screen.findByText(/licence text has changed since you accepted it/);
    const box = screen.getByRole("checkbox") as HTMLInputElement;
    expect(box.checked).toBe(false);
    fireEvent.click(box);
    expect(onChange).toHaveBeenCalledWith(SHA_B);
  });

  test("an accepted licence stays accepted while the text is pending, and can be revoked", async () => {
    eulaResponses = [eulaOf({ state: "pending", text: null, textSha256: null })];
    const onChange = renderEula(SHA_A);
    await screen.findByRole("progressbar");
    const box = screen.getByRole("checkbox") as HTMLInputElement;
    expect(box.checked).toBe(true);
    expect(screen.queryByText(/has changed/)).toBeNull();
    fireEvent.click(box);
    expect(onChange).toHaveBeenCalledWith(null);
  });
});

/** Next through the names, drivers and review steps to the save. */
async function toReview() {
  for (const h of ["Names and locations", "Drivers and licences"]) {
    await heading(h);
    press("Next");
  }
  await heading("Review and save");
}

describe("Setup wizard", () => {
  test("a step change fades the new step in as a new node; the first step and the heading do not", async () => {
    mockServer(() => hpSetup);
    mount();
    await heading("Choose devices");
    await waitFor(() => deviceItem("HP LaserJet Professional M1132 MFP"));
    const h1 = screen.getByRole("heading", { level: 1 });
    const first = document.querySelector(".setup-step");
    expect(first).toBeTruthy();
    expect(first?.hasAttribute("data-arrived")).toBe(false);

    press("Next");
    await heading("Names and locations");
    const second = document.querySelector(".setup-step");
    expect(second).not.toBe(first);
    expect(second?.hasAttribute("data-arrived")).toBe(true);
    // The heading (the focus target after navigation) and the buttons stay mounted.
    expect(screen.getByRole("heading", { level: 1 })).toBe(h1);
    expect(second?.contains(screen.getByRole("button", { name: "Next" }))).toBe(false);

    press("Back");
    await heading("Choose devices");
    const third = document.querySelector(".setup-step");
    expect(third).not.toBe(second);
    expect(third?.hasAttribute("data-arrived")).toBe(true);
  });

  test("first run: an MFP as printer and scanner, names, licence, review, ready, defaults, done", async () => {
    // A hook-order mistake across the flows shows only as a React console error.
    const consoleError = spyOn(console, "error");
    try {
      mockServer(() => hpSetup);
      mount();
      await heading("Choose devices");
      const item = await waitFor(() => deviceItem("HP LaserJet Professional M1132 MFP"));
      expect(item.textContent).toContain("USB 03f0:042a");
      // Every role starts as "Use".
      for (const name of ["As a printer", "As a scanner"]) {
        const group = within(item).getByRole("radiogroup", { name });
        expect(
          (within(group).getByRole("radio", { name: /^Use as/ }) as HTMLInputElement).checked,
        ).toBe(true);
      }
      expect(screen.getByRole("list", { name: "Setup steps" })).toBeTruthy();
      expect(document.querySelector('[aria-current="step"]')?.textContent).toContain(
        "Choose devices",
      );

      press("Next");
      await heading("Names and locations");
      expect(window.location.hash).toBe("#names");
      expect(screen.getByRole("heading", { level: 1 }).getAttribute("data-document-title")).toBe(
        "Step 2 of 5: Names and locations",
      );
      // The installation name follows the first chosen device.
      const title = screen.getByRole("textbox", { name: /Installation name/ }) as HTMLInputElement;
      expect(title.value).toBe("HP LaserJet Professional M1132 MFP");
      const [printerName, scannerName] = screen.getAllByRole("textbox", {
        name: /^Name/,
      }) as HTMLInputElement[];
      expect(printerName?.value).toBe("HP LaserJet Professional M1132 MFP");
      expect(scannerName?.value).toBe("HP LaserJet Professional M1132 MFP scanner");
      // A visible marker says the field is required.
      expect(
        printerName?.closest(".ui-field")?.querySelector(".ui-required")?.textContent,
      ).toContain("required");
      fireEvent.change(printerName as HTMLInputElement, { target: { value: "" } });
      press("Next");
      expect((await screen.findAllByText(/Enter 1 to 63 characters/)).length).toBeGreaterThan(0);
      // Next moves focus to the first field with an error.
      await waitFor(() => expect(document.activeElement).toBe(printerName as HTMLInputElement));
      expect(window.location.hash).toBe("#names");
      fireEvent.change(printerName as HTMLInputElement, { target: { value: "Hall printer" } });
      fireEvent.change(title, { target: { value: "Home" } });
      fireEvent.change(screen.getAllByRole("textbox", { name: /^Location/ })[0] as HTMLElement, {
        target: { value: "Hall" },
      });
      press("Next");

      await heading("Drivers and licences");
      expect(screen.getByRole("button", { name: /Printer driver/ }).textContent).toContain(
        "Automatic: HP (HPLIP)",
      );
      // How the driver was chosen is jargon: it sits in a closed disclosure.
      expect(screen.getByText("Printer model").closest("[hidden]")).toBeTruthy();
      expect((await screen.findByRole("region", { name: /licence text/ })).textContent).toContain(
        "HP PLUG-IN LICENSE AGREEMENT",
      );
      fireEvent.click(screen.getByRole("checkbox", { name: /accept the HP Linux Imaging/ }));
      press("Next");

      await heading("Review and save");
      // Back keeps the edits.
      press("Back");
      await heading("Drivers and licences");
      press("Next");
      await heading("Review and save");
      expect(document.body.textContent).toContain("Hall printer (Hall)");
      expect(document.body.textContent).toContain("HP LaserJet Professional M1132 MFP scanner");
      press("Save");
      // No pending state on a button that unmounts: its announcement would outlive it.
      expect(screen.queryByRole("button", { name: "Save" })?.hasAttribute("data-pending")).not.toBe(
        true,
      );

      await heading("Starting the devices");
      const put = lastCall("PUT");
      expect(put?.headers["If-None-Match"]).toBe("*");
      const body = put?.body as Config;
      expect(body.apiVersion).toBe("pagewire/v2");
      expect(body.name).toBe("Home");
      expect(body.printers).toEqual([
        { ...printerEntry("hp", "Hall printer", hpDevice), location: "Hall" },
      ]);
      expect(body.scanners).toEqual([
        scannerEntry("hp-scanner", "HP LaserJet Professional M1132 MFP scanner", hpDevice),
      ]);
      expect(body.ignoredDevices).toEqual([]);
      expect(body.licenses.acceptedEulas).toEqual([
        { id: "hplip-plugin", version: "3.24.4", textSha256: SHA_A },
      ]);

      const printer = body.printers[0] as Printer;
      const scanner = body.scanners[0] as Scanner;
      FakeEventSource.last.status(
        applied({
          printers: [
            statusPrinter(printer, hpCaps, {
              phase: "deploying",
              message: "Waiting for the device agent",
            }),
          ],
          scanners: [statusScanner(scanner, { phase: "deploying" })],
        }),
      );
      expect(await screen.findByText("Waiting for the device agent")).toBeTruthy();
      FakeEventSource.last.status(
        applied({
          printers: [
            statusPrinter(printer, hpCaps, {
              connect: {
                dnssdName: "Hall printer",
                ippUris: ["ipp://192.168.1.20:8002/ipp/print/Hall_printer"],
              },
            }),
          ],
          scanners: [
            statusScanner(scanner, {
              connect: { dnssdName: "", esclUrls: ["http://192.168.1.20:8090/eSCL"] },
            }),
          ],
        }),
      );
      await heading("Defaults");
      // Nothing differs from the devices' own defaults, so there is nothing to save.
      press("Keep these");
      await heading("pagewire is ready");
      expect(calls.some((c) => c.method === "PATCH")).toBe(false);
      expect(document.body.textContent).toContain("ipp://192.168.1.20:8002/ipp/print/Hall_printer");
      expect(document.body.textContent).toContain("http://192.168.1.20:8090/eSCL");
      expect(screen.getByRole("link", { name: "Go to Status" }).getAttribute("href")).toBe("/");
      expect(consoleError).not.toHaveBeenCalled();
    } finally {
      consoleError.mockRestore();
    }
  });

  test("Don't use for a role records it in ignoredDevices; the keyboard is folded away", async () => {
    mockServer(() => setupOf([kbd, scx]));
    mount();
    await heading("Choose devices");
    await waitFor(() => deviceItem("SCX-4300 Series"));
    const toggle = screen.getByRole("button", { name: /Show 1 other USB device\b/ });
    expect(toggle.getAttribute("aria-expanded")).toBe("false");
    expect(screen.getByText("USB Keyboard").closest("[hidden]")).toBeTruthy();
    decline("SCX-4300 Series", "scanner");
    press("Next");
    await toReview();
    expect(document.body.textContent).toContain("Not used");
    press("Save");
    await heading("Starting the devices");
    const body = lastCall("PUT").body as Config;
    expect(body.printers.map((p) => p.id)).toEqual(["scx-4300"]);
    expect(body.scanners).toEqual([]);
    expect(body.ignoredDevices).toEqual([
      { vendorId: "04e8", productId: "342e", serial: "SCX0001", roles: ["scanner"] },
    ]);
    // No licence was shown for the Samsung, so none is written.
    expect(body.licenses.acceptedEulas).toEqual([]);
  });

  test("Don't use for everything still saves and lands on Status", async () => {
    mockServer(() => hpSetup);
    mount();
    await heading("Choose devices");
    await waitFor(() => deviceItem("HP LaserJet Professional M1132 MFP"));
    decline("HP LaserJet Professional M1132 MFP", "printer");
    decline("HP LaserJet Professional M1132 MFP", "scanner");
    press("Next");
    await toReview();
    press("Save");
    await heading("Starting the devices");
    // The shell sends every page back to setup while the config is missing: wait for it.
    expect(window.location.pathname).toBe("/setup");
    FakeEventSource.last.status(applied({}));
    await waitFor(() => expect(window.location.pathname).toBe("/"));
    const body = lastCall("PUT").body as Config;
    expect(body.printers).toEqual([]);
    expect(body.scanners).toEqual([]);
    expect(body.ignoredDevices).toEqual([
      { vendorId: "03f0", productId: "042a", serial: "FAKE0001", roles: ["printer", "scanner"] },
    ]);
  });

  test("defaults step: the Samsung's sides is disabled with its reason, and controls are listboxes", async () => {
    mockServer(() => setupOf([scx]));
    mount();
    await heading("Choose devices");
    await waitFor(() => deviceItem("SCX-4300 Series"));
    decline("SCX-4300 Series", "scanner");
    press("Next");
    await toReview();
    press("Save");
    await heading("Starting the devices");
    const printer = (lastCall("PUT").body as Config).printers[0] as Printer;
    FakeEventSource.last.status(applied({ printers: [statusPrinter(printer, samsungCaps)] }));
    await heading("Defaults");
    expect(screen.getByRole("heading", { level: 1 }).getAttribute("data-document-title")).toBe(
      "Step 5 of 5: Defaults",
    );
    const sides = screen.getByRole("button", { name: /Sides/ });
    expect(sides.getAttribute("aria-haspopup")).toBe("listbox");
    expect(sides.hasAttribute("disabled")).toBe(true);
    expect(description(sides)).toContain("This printer only prints one-sided.");
    const quality = screen.getByRole("button", { name: /Quality/ });
    expect(quality.getAttribute("aria-haspopup")).toBe("listbox");
    expect(quality.hasAttribute("disabled")).toBe(false);
    // Every kind shows, the one-value ones disabled.
    expect(screen.getByRole("button", { name: /Paper tray/ }).hasAttribute("disabled")).toBe(true);
    expect(document.querySelector('[role="radiogroup"]')).toBeNull();
  });

  test("defaults Save re-reads the config and changes only the edited key, under If-Match", async () => {
    mockServer(() => setupOf([hp, scx]));
    mount();
    await heading("Choose devices");
    await waitFor(() => deviceItem("SCX-4300 Series"));
    decline("HP LaserJet Professional M1132 MFP", "scanner");
    decline("SCX-4300 Series", "scanner");
    press("Next");
    await toReview();
    press("Save");
    await heading("Starting the devices");
    const [hpEntry, scxEntry] = (lastCall("PUT").body as Config).printers as [Printer, Printer];
    // Since the wizard saved, someone gave both printers defaults: they must survive.
    const onDisk: Config = {
      ...emptyConfig("Home"),
      printers: [
        { ...hpEntry, defaults: { sides: "two-sided-long-edge" } },
        { ...scxEntry, location: "Office", defaults: { colorMode: "monochrome" } },
      ],
    };
    configResponses = [configResponse(onDisk, "e7")];
    FakeEventSource.last.status(
      applied({
        printers: [statusPrinter(hpEntry, hpCaps), statusPrinter(scxEntry, samsungCaps)],
      }),
    );
    await heading("Defaults");
    const samsungCard = screen
      .getByRole("heading", { level: 2, name: "SCX-4300 Series" })
      .closest("section") as HTMLElement;
    await choose(/Quality/, "High", samsungCard);
    press("Save");
    await heading("pagewire is ready");
    const patch = lastCall("PATCH");
    expect(patch?.headers["If-Match"]).toBe('"e7"');
    const sent = patch?.body as { printers: Printer[]; scanners?: unknown };
    expect(Object.keys(sent)).toEqual(["printers"]);
    expect(JSON.stringify(sent.printers[0])).toBe(JSON.stringify(onDisk.printers[0]));
    expect(sent.printers[1]).toEqual({
      ...(onDisk.printers[1] as Printer),
      defaults: { colorMode: "monochrome", quality: "high" },
    });
  });

  test("defaults: the locale's paper size is preselected when the printer offers it", async () => {
    mockServer(() => setupOf([scx]));
    mount();
    await heading("Choose devices");
    await waitFor(() => deviceItem("SCX-4300 Series"));
    decline("SCX-4300 Series", "scanner");
    press("Next");
    await toReview();
    press("Save");
    await heading("Starting the devices");
    const entry = (lastCall("PUT").body as Config).printers[0] as Printer;
    configResponses = [configResponse({ ...emptyConfig("Home"), printers: [entry] }, "e3")];
    // This printer defaults to A4; happy-dom reports en-US, which expects Letter.
    const a4First = { ...samsungCaps, mediaDefault: "iso_a4_210x297mm" };
    FakeEventSource.last.status(applied({ printers: [statusPrinter(entry, a4First)] }));
    await heading("Defaults");
    expect(screen.getByRole("button", { name: /Paper size/ }).textContent).toContain("Letter");
    press("Save");
    await heading("pagewire is ready");
    expect((lastCall("PATCH").body as Config).printers[0]?.defaults).toEqual({
      media: "na_letter_8.5x11in",
    });
  });

  test("defaults: the printer default entry removes the key, and a 412 offers Save again", async () => {
    mockServer(() => setupOf([scx]));
    mount();
    await heading("Choose devices");
    await waitFor(() => deviceItem("SCX-4300 Series"));
    decline("SCX-4300 Series", "scanner");
    press("Next");
    await toReview();
    press("Save");
    await heading("Starting the devices");
    const entry = (lastCall("PUT").body as Config).printers[0] as Printer;
    const onDisk = {
      ...emptyConfig("Home"),
      printers: [{ ...entry, defaults: { quality: "draft" as const } }],
    };
    configResponses = [configResponse(onDisk, "e3"), configResponse(onDisk, "e4")];
    let first = true;
    patchResponse = () => {
      if (!first) return json(configResponse(onDisk, "e5"));
      first = false;
      return json(configResponse(onDisk, "e4"), 412);
    };
    FakeEventSource.last.status(applied({ printers: [statusPrinter(entry, samsungCaps)] }));
    await heading("Defaults");
    await choose(/Resolution/, "300 dpi");
    await choose(/Quality/, "High");
    await choose(/Quality/, "Printer default (Normal)");
    press("Save");
    expect(await screen.findByText(/Someone else changed the settings/)).toBeTruthy();
    press("Save again");
    await heading("pagewire is ready");
    const patches = calls.filter((c) => c.method === "PATCH");
    expect(patches.map((c) => c.headers["If-Match"])).toEqual(['"e3"', '"e4"']);
    expect((lastCall("PATCH").body as { printers: Printer[] }).printers[0]?.defaults).toEqual({
      resolution: 300,
    });
  });

  test("add devices: PATCH joins the file as it is at save time; ignored devices can be offered again", async () => {
    window.history.replaceState(null, "", "/setup?add=1");
    const samsung = printerEntry("scx", "Samsung", scxDevice);
    const current: Config = {
      ...emptyConfig("Home"),
      printers: [samsung],
      ignoredDevices: [
        { vendorId: "04e8", productId: "342e", serial: "SCX0001", roles: ["scanner"] },
      ],
    };
    const later: Config = { ...current, printers: [{ ...samsung, location: "Office" }] };
    configResponses = [configResponse(current, "e8"), configResponse(later, "e9")];
    const setup = setupOf(
      [{ ...scx, printerId: "scx", ignored: ["scanner"], suggestedPrinter: null }, hp],
      { required: false },
    );
    mockServer(() => setup);
    mount();
    await heading("Choose devices");
    FakeEventSource.last.status(applied({ printers: [statusPrinter(samsung, samsungCaps)] }));
    expect(await screen.findByText("In use as printer Samsung")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Not offered (1)" })).toBeTruthy();
    // HP: printer only.
    decline("HP LaserJet Professional M1132 MFP", "scanner");
    press("Next");
    await heading("Names and locations");
    // Adding keeps the installation name.
    expect(screen.queryByRole("textbox", { name: /Installation name/ })).toBeNull();
    press("Back");
    await heading("Choose devices");
    // Offer the Samsung's scanner again and use it.
    fireEvent.click(screen.getByRole("button", { name: "Not offered (1)" }));
    press("Offer again");
    await waitFor(() =>
      expect(
        within(deviceItem("SCX-4300 Series")).getByRole("radiogroup", { name: "As a scanner" }),
      ).toBeTruthy(),
    );
    press("Next");
    await toReview();
    press("Save");
    await heading("Starting the devices");
    const patch = lastCall("PATCH");
    expect(patch?.headers["If-Match"]).toBe('"e9"');
    const body = patch?.body as Config;
    expect(body.printers).toEqual([
      { ...samsung, location: "Office" },
      printerEntry("hp", "HP LaserJet Professional M1132 MFP", hpDevice),
    ]);
    expect(body.scanners).toEqual([
      scannerEntry("scx-4300-scanner", "SCX-4300 Series scanner", scxDevice),
    ]);
    expect(body.ignoredDevices).toEqual([
      { vendorId: "03f0", productId: "042a", serial: "FAKE0001", roles: ["scanner"] },
    ]);
    expect(calls.some((c) => c.method === "PUT")).toBe(false);
  });

  test("add devices: a suggested id already in use gets a suffix", async () => {
    window.history.replaceState(null, "", "/setup?add=1");
    const other = printerEntry("hp", "Old HP", { ...hpDevice, serial: "OTHER" });
    configResponses = [configResponse({ ...emptyConfig("Home"), printers: [other] }, "e1")];
    mockServer(() => setupOf([hp], { required: false }));
    mount();
    await heading("Choose devices");
    await waitFor(() => deviceItem("HP LaserJet Professional M1132 MFP"));
    decline("HP LaserJet Professional M1132 MFP", "scanner");
    press("Next");
    await toReview();
    press("Save");
    await heading("Starting the devices");
    expect((lastCall("PATCH").body as Config).printers.map((p) => p.id)).toEqual(["hp", "hp-2"]);
  });

  test("add devices with an invalid settings file says to fix it first", async () => {
    window.history.replaceState(null, "", "/setup?add=1");
    configResponses = [
      { ...configResponse(emptyConfig("x"), "e1"), config: null, state: "invalid" },
    ];
    mockServer(() => setupOf([hp], { required: false }));
    mount();
    expect(await screen.findByText(/The settings file has an error/)).toBeTruthy();
  });

  test("Detect again asks the server to look again", async () => {
    mockServer(() => hpSetup);
    mount();
    await heading("Choose devices");
    await waitFor(() => deviceItem("HP LaserJet Professional M1132 MFP"));
    press("Detect again");
    expect(await screen.findByText(/Looking again/)).toBeTruthy();
    expect(calls.some((c) => c.method === "POST" && c.url.endsWith("/setup/detect"))).toBe(true);
  });

  test("no-USB path: prerequisites, then a simulated printer and scanner", async () => {
    mockServer(() => noUsbSetup);
    mount();
    await heading("Choose devices");
    expect(await screen.findByText("No USB devices available")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Next" }).hasAttribute("disabled")).toBe(true);
    press("Use a simulated printer");
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Next" }).hasAttribute("disabled")).toBe(false),
    );
    press("Next");
    await heading("Names and locations");
    press("Next");
    await heading("Drivers and licences");
    expect(document.body.textContent).toContain("need no driver");
    press("Next");
    await heading("Review and save");
    press("Save");
    await heading("Starting the devices");
    const body = lastCall("PUT").body as Config;
    expect(body.name).toBe("Printer");
    expect(body.printers).toEqual([
      {
        id: "printer",
        name: "Printer",
        location: "",
        enabled: true,
        mode: "simulated",
        driver: "auto",
        defaults: {},
      },
    ]);
    expect(body.scanners.map((x) => [x.id, x.name, x.mode])).toEqual([
      ["scanner", "Printer scanner", "simulated"],
    ]);
  });

  test("waiting shows the probe's message, then the devices when the probe reports", async () => {
    let current = setupOf([]);
    mockServer(() => current);
    mount();
    await heading("Choose devices");
    FakeEventSource.last.status({
      ...initialStatus,
      phase: "probing",
      message: "Printer not plugged in or not visible to the device plugin",
    });
    expect(
      await screen.findByText("Printer not plugged in or not visible to the device plugin"),
    ).toBeTruthy();
    expect(document.body.textContent).toContain("Plug the printer in");
    expect(screen.getByRole("button", { name: "Next" }).hasAttribute("disabled")).toBe(true);

    current = hpSetup;
    FakeEventSource.last.status({ ...initialStatus, phase: "probing", devices: [hp] });
    await waitFor(() => deviceItem("HP LaserJet Professional M1132 MFP"));
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Next" }).hasAttribute("disabled")).toBe(false),
    );
  });

  test("only devices pagewire cannot use: says so and Next stays off", async () => {
    mockServer(() => setupOf([kbd]));
    mount();
    await heading("Choose devices");
    expect(await screen.findByText(/None of these devices is a printer or scanner/)).toBeTruthy();
    expect(screen.getByRole("button", { name: /Show 1 other USB device\b/ })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Next" }).hasAttribute("disabled")).toBe(true);
  });

  test("a first-run 412 says another session finished setup and reloads", async () => {
    mockServer(() => noUsbSetup);
    putResponse = () => json(configResponse(emptyConfig("Other"), "x"), 412);
    const reload = mock(() => {});
    mount({ reload, reloadDelayMs: 10 });
    await heading("Choose devices");
    press("Use a simulated printer");
    press("Next");
    await toReview();
    press("Save");
    expect(await screen.findByText("Another session finished setup. Reloading…")).toBeTruthy();
    await waitFor(() => expect(reload).toHaveBeenCalledTimes(1));
  });

  test("waiting for ready times out with a message", async () => {
    mockServer(() => noUsbSetup);
    mount({ readyTimeoutMs: 20 });
    await heading("Choose devices");
    press("Use a simulated printer");
    press("Next");
    await toReview();
    press("Save");
    await heading("Starting the devices");
    expect(await screen.findByText(/longer than 3 minutes/)).toBeTruthy();
  });

  test("a 422 lists the rejected fields and stays on Review", async () => {
    mockServer(() => noUsbSetup);
    putResponse = () =>
      json(
        {
          type: "about:blank",
          title: "Unprocessable",
          status: 422,
          detail: "invalid config",
          errors: [{ path: "/printers/0/name", line: 2, column: 7, message: "too long" }],
        },
        422,
      );
    mount();
    await heading("Choose devices");
    press("Use a simulated printer");
    press("Next");
    await toReview();
    press("Save");
    expect((await screen.findByRole("alert")).textContent).toContain("/printers/0/name: too long");
    expect(screen.getByRole("heading", { level: 1 }).textContent).toBe("Review and save");
  });

  test("a printer no driver matches: the device ID on Drivers and Review", async () => {
    const deviceId = "MFG:Zebra;CMD:ZPL;MDL:GK420d;CLS:PRINTER;";
    const device = { ...hpDevice, manufacturer: "Zebra", product: "GK420d", deviceId };
    mockServer(() =>
      setupOf(
        [
          detected(device, "gk420d", {
            canScan: false,
            suggestedScanner: null,
            scannerDecision: null,
            printerDecision: {
              ...hpDecision,
              family: "",
              driver: "",
              driverDescription: "",
              optionalEulas: [],
              rule: "none",
              device,
            },
          }),
        ],
        { eulas: [] },
      ),
    );
    mount();
    await heading("Choose devices");
    await waitFor(() => deviceItem("GK420d"));
    press("Next");
    await heading("Names and locations");
    press("Next");
    await heading("Drivers and licences");
    expect(screen.getByText("No printer driver matches this printer")).toBeTruthy();
    expect(screen.getByText(deviceId).tagName).toBe("CODE");
    expect(screen.getByRole("button", { name: /Printer driver/ }).textContent).toContain(
      "Automatic: no driver matches",
    );
    press("Next");
    await heading("Review and save");
    expect(screen.getByText(deviceId)).toBeTruthy();
    expect(document.body.textContent).toContain("After setup, choose a driver in Settings.");
  });

  test("a simulated inventory is labelled simulated", async () => {
    const sim = detected(hpDevice, "hp", {
      suggestedPrinter: { ...printerEntry("hp", hpDevice.product, hpDevice), mode: "simulated" },
    });
    mockServer(() => setupOf([sim]));
    mount();
    await heading("Choose devices");
    expect(await screen.findByText("HP LaserJet Professional M1132 MFP (simulated)")).toBeTruthy();
  });

  test("a reload on a later step starts again at the first step", async () => {
    window.history.replaceState(null, "", "/setup#review");
    mockServer(() => hpSetup);
    mount();
    await heading("Choose devices");
  });

  test("an existing config without ?add=1 shows the already-set-up note", async () => {
    mockServer(() => ({ ...hpSetup, required: false }));
    mount();
    await heading("pagewire is already set up");
    expect(screen.getByRole("link", { name: "Go to Settings" }).getAttribute("href")).toBe(
      "/settings",
    );
    expect(screen.getByRole("link", { name: "Detect and add devices" }).getAttribute("href")).toBe(
      "/setup?add=1",
    );
  });
});
