import { afterEach, beforeEach, describe, expect, test } from "bun:test";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { createEventStore, type EventStore, EventsContext } from "../api/events";
import {
  type BlobStatus,
  type Config,
  type ConfigResponse,
  ConfigSchema,
  type DetectedDevice,
  type PrinterCaps,
  type ScannerCaps,
  type Setup as SetupDTO,
  type Status,
  type StatusPrinter,
  type StatusScanner,
  StatusSchema,
} from "../gen/api";
import { navigate } from "../router";
import { Settings } from "./Settings";

const SHA = "c".repeat(64);
const HPLIP = { id: "hplip-plugin", version: "3.24.4" };
const HPLIP_ACCEPTED = { ...HPLIP, textSha256: SHA };

const samsung: Config["printers"][number] = {
  id: "scx-4300",
  name: "Samsung SCX-4300",
  location: "Study",
  enabled: true,
  mode: "usb",
  usb: { vendorId: "04e8", productId: "342e", serial: "" },
  driver: "auto",
  defaults: {},
};
const hp: Config["printers"][number] = {
  id: "officejet",
  name: "HP OfficeJet",
  location: "",
  enabled: true,
  mode: "usb",
  usb: { vendorId: "03f0", productId: "2b17", serial: "CN123" },
  driver: "auto",
  defaults: {},
};
const lide: Config["scanners"][number] = {
  id: "lide",
  name: "CanoScan LiDE 100",
  location: "",
  enabled: true,
  mode: "usb",
  usb: { vendorId: "04a9", productId: "1904", serial: "" },
  backend: "auto",
  defaults: {},
};
const keyboard = { vendorId: "046d", productId: "c31c", serial: "", roles: ["printer" as const] };

/** A v2 config; parsed, so a fixture that the server could never send fails loudly. */
function config(over: Partial<Config> = {}): Config {
  return ConfigSchema.parse({
    apiVersion: "pagewire/v2",
    name: "Home",
    printers: [samsung, hp],
    scanners: [lide],
    ignoredDevices: [keyboard],
    print: { maxUploadMiB: 50 },
    scan: { retentionDays: 7 },
    licenses: { acceptedEulas: [HPLIP_ACCEPTED] },
    ...over,
  });
}
const withPrinter = (i: number, over: Partial<Config["printers"][number]>) => {
  const c = config();
  return config({ printers: c.printers.map((p, j) => (j === i ? { ...p, ...over } : p)) });
};

const applied = (etag: string, c: Config = config()): ConfigResponse => ({
  etag,
  config: c,
  state: "applied",
  errors: [],
  lastGood: null,
});
const invalid = (etag: string, lastGood: Config | null = null): ConfigResponse => ({
  etag,
  config: null,
  state: "invalid",
  errors: [{ path: "", line: 3, column: 1, message: "mapping values are not allowed here" }],
  lastGood,
});

const setup: SetupDTO = {
  required: false,
  backend: "sim",
  usbResourceConfigured: true,
  devices: [],
  eulas: [
    {
      ...HPLIP,
      title: "HP Linux Imaging and Printing proprietary plug-in",
      vendor: "HP",
      state: "available",
      textSha256: SHA,
      summary: "Needed by some HP printers.",
      required: false,
    },
  ],
  suggested: config({ printers: [], scanners: [], ignoredDevices: [] }),
};

const noCaps: PrinterCaps = {
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
// The Samsung as the sim reports it: monochrome, one-sided, no trays.
const samsungCaps: PrinterCaps = {
  ...noCaps,
  media: ["na_letter_8.5x11in", "iso_a4_210x297mm"],
  sides: ["one-sided"],
  colorModes: ["monochrome"],
  qualities: ["draft", "normal", "high"],
  resolutions: [300, 600],
  copiesMax: 99,
  mediaDefault: "na_letter_8.5x11in",
  sidesDefault: "one-sided",
  colorModeDefault: "monochrome",
  qualityDefault: "normal",
  resolutionDefault: 600,
};
const lideCaps: ScannerCaps = {
  sources: ["platen"],
  resolutions: [75, 150, 300, 600],
  colorModes: ["color", "grayscale"],
  adf: false,
  duplex: false,
  intents: [],
};

const printerStatus = (p: { id: string; name: string }, over: Partial<StatusPrinter> = {}) =>
  ({
    id: p.id,
    name: p.name,
    location: "",
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
    capabilities: noCaps,
    connect: { dnssdName: "", ippUris: [] },
    ...over,
  }) satisfies StatusPrinter;
const scannerStatus = (s: { id: string; name: string }, over: Partial<StatusScanner> = {}) =>
  ({
    id: s.id,
    name: s.name,
    location: "",
    enabled: true,
    phase: "ready",
    message: "",
    warnings: [],
    decision: null,
    device: null,
    available: true,
    url: "",
    state: "idle",
    capabilities: lideCaps,
    connect: { dnssdName: "", esclUrls: [] },
    ...over,
  }) satisfies StatusScanner;

const brother: DetectedDevice = {
  device: {
    vendorId: "04f9",
    productId: "0042",
    serial: "E7",
    manufacturer: "Brother",
    product: "HL-L2350DW",
    bus: 1,
    address: 12,
    interfaces: [],
    deviceId: "",
  },
  node: "",
  canPrint: true,
  canScan: false,
  printerId: "",
  scannerId: "",
  ignored: [],
  suggestedPrinter: null,
  suggestedScanner: null,
  printerDecision: null,
  scannerDecision: null,
};

function status(
  over: { printers?: StatusPrinter[]; blobs?: BlobStatus[]; devices?: DetectedDevice[] } = {},
): Status {
  return StatusSchema.parse({
    config: {
      state: "applied",
      etag: "e1",
      appliedEtag: "e1",
      errors: [],
      changedAt: "2026-11-24T12:00:00+01:00",
    },
    phase: "ready",
    message: "",
    printers: over.printers ?? [
      printerStatus(samsung, { capabilities: samsungCaps }),
      printerStatus(hp, { available: false, phase: "probing" }),
    ],
    scanners: [scannerStatus(lide)],
    workloads: [
      {
        node: "",
        phase: "running",
        mode: "usb",
        message: "",
        advertised: [],
        blobs: over.blobs ?? [],
      },
    ],
    devices: over.devices ?? [],
    reconcile: [],
  });
}

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
  emit(type: string, data: unknown) {
    act(() => {
      this.listeners.get(type)?.(new MessageEvent(type, { data: JSON.stringify(data) }));
    });
  }
}

type Call = { method: string; url: string; headers: Record<string, string>; body: unknown };
let calls: Call[];
let getConfig: () => Response;
let writes: Array<() => Response>;
// The sha256 of the licence text the agent reports now.
let eulaSha: string;
const realFetch = globalThis.fetch;
const json = (body: unknown, status = 200) =>
  new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
const problem = (status: number, type: string, errors: unknown[] = []) =>
  json({ type, title: "x", status, detail: "", errors }, status);

beforeEach(() => {
  calls = [];
  writes = [];
  eulaSha = SHA;
  getConfig = () => json(applied("e1"));
  globalThis.fetch = (async (input: RequestInfo | URL, init: RequestInit = {}) => {
    const url = String(input);
    const method = init.method ?? "GET";
    calls.push({
      method,
      url,
      headers: (init.headers ?? {}) as Record<string, string>,
      body: init.body ? JSON.parse(String(init.body)) : undefined,
    });
    if (url.endsWith("/api/v1/setup")) return json(setup);
    if (url.endsWith("/api/v1/eulas/hplip-plugin"))
      return json({
        ...HPLIP,
        title: "HP Linux Imaging and Printing proprietary plug-in",
        vendor: "HP",
        summary: "Needed by some HP printers.",
        state: "available",
        message: "",
        textSha256: eulaSha,
        text: `Licence ${eulaSha.slice(0, 4)}`,
      });
    if (url.endsWith("/api/v1/config") && method === "GET") return getConfig();
    if (url.endsWith("/api/v1/config")) {
      const next = writes.shift();
      if (next) return next();
    }
    return problem(404, "about:blank");
  }) as unknown as typeof fetch;
});

let store: EventStore;
afterEach(() => {
  cleanup();
  store?.stop();
  globalThis.fetch = realFetch;
});

async function mount(live: Status | null = status()) {
  store = createEventStore({
    newEventSource: () => new FakeEventSource() as unknown as EventSource,
  });
  store.start();
  render(
    <EventsContext value={store}>
      <Settings />
    </EventsContext>,
  );
  await screen.findByRole("heading", { level: 1, name: "Settings" });
  if (live) FakeEventSource.last.emit("status", live);
}

const press = (el: HTMLElement) =>
  act(async () => {
    fireEvent.click(el);
  });
const button = (name: string | RegExp, scope: HTMLElement = document.body) =>
  within(scope).getByRole("button", { name });
const field = (name: string, scope: HTMLElement = document.body) =>
  within(scope).getByRole("textbox", { name }) as HTMLInputElement;
const type = async (name: string, value: string, scope: HTMLElement = document.body) => {
  const el = await within(scope).findByRole("textbox", { name });
  act(() => {
    fireEvent.change(el, { target: { value } });
  });
};
const writesOf = (method: string) => calls.filter((c) => c.method === method);
const save = async () => press(button("Save"));

/** One printer's or scanner's section, opened for editing. */
async function edit(name: string) {
  const entry = await screen.findByRole("region", { name });
  await press(button("Edit", entry));
  return entry;
}
/** The text of the elements `el`'s aria-describedby points at. */
const description = (el: HTMLElement) =>
  (el.getAttribute("aria-describedby") ?? "")
    .split(" ")
    .filter(Boolean)
    .map((id) => document.getElementById(id)?.textContent ?? "")
    .join(" ");
const isDisabled = (el: HTMLElement) =>
  el.hasAttribute("disabled") || el.getAttribute("aria-disabled") === "true";
const choose = async (trigger: HTMLElement, option: string) => {
  await press(trigger);
  await press(await screen.findByRole("option", { name: option }));
};

describe("Settings form", () => {
  test("shows each section and sends edits as a merge patch with If-Match", async () => {
    await mount();
    for (const name of ["General", "Printers", "Scanners", "Ignored devices", "Licences"])
      expect(await screen.findByRole("heading", { level: 2, name })).toBeTruthy();
    expect((button("Save") as HTMLButtonElement).disabled).toBe(true);

    await type("Title", "Office");
    writes.push(() => json(applied("e2", config({ name: "Office" }))));
    await save();

    const [patch] = writesOf("PATCH");
    expect(patch?.body).toEqual({ name: "Office" });
    expect(patch?.headers["If-Match"]).toBe('"e1"');
    expect(patch?.headers["Content-Type"]).toBe("application/merge-patch+json");
    await waitFor(() => expect((button("Save") as HTMLButtonElement).disabled).toBe(true));
    expect(field("Title").value).toBe("Office");
  });

  test("links to detecting and adding devices", async () => {
    await mount();
    const link = await screen.findByRole("link", { name: "Detect and add devices" });
    expect(link.getAttribute("href")).toBe("/setup?add=1");
  });

  test("lists printers and scanners with their status; entries stay closed until edited", async () => {
    await mount();
    const entry = await screen.findByRole("region", { name: "Samsung SCX-4300" });
    expect(entry.textContent).toContain("Study");
    expect(entry.textContent).toContain("Ready");
    expect(within(entry).queryByRole("textbox", { name: "Name" })).toBeNull();
    expect(screen.getByRole("region", { name: "CanoScan LiDE 100" })).toBeTruthy();
    await press(button("Edit", entry));
    expect(field("Name", entry).value).toBe("Samsung SCX-4300");
  });

  test("switching a printer off sends the whole printers list with it disabled", async () => {
    await mount();
    const entry = await screen.findByRole("region", { name: "HP OfficeJet" });
    act(() => {
      fireEvent.click(within(entry).getByRole("switch", { name: "In use" }));
    });
    writes.push(() => json(applied("e2", withPrinter(1, { enabled: false }))));
    await save();
    expect(writesOf("PATCH")[0]?.body).toEqual({
      printers: [samsung, { ...hp, enabled: false }],
    });
  });

  test("removing a printer asks first, then saves the list without it", async () => {
    await mount();
    const entry = await screen.findByRole("region", { name: "HP OfficeJet" });
    await press(button("Remove", entry));
    const dialog = await screen.findByRole("alertdialog", { name: "Remove HP OfficeJet?" });
    await press(button("Cancel", dialog));
    expect(screen.getByRole("region", { name: "HP OfficeJet" })).toBeTruthy();

    await press(button("Remove", screen.getByRole("region", { name: "HP OfficeJet" })));
    await press(
      button("Remove", await screen.findByRole("alertdialog", { name: "Remove HP OfficeJet?" })),
    );
    expect(screen.queryByRole("region", { name: "HP OfficeJet" })).toBeNull();
    writes.push(() => json(applied("e2", config({ printers: [samsung] }))));
    await save();
    expect(writesOf("PATCH")[0]?.body).toEqual({ printers: [samsung] });
  });

  test("Offer again removes the device from the ignored list", async () => {
    await mount();
    const card = (
      await screen.findByRole("heading", { level: 2, name: "Ignored devices" })
    ).closest("section") as HTMLElement;
    expect(card.textContent).toContain("046d:c31c");
    await press(button("Offer again", card));
    writes.push(() => json(applied("e2", config({ ignoredDevices: [] }))));
    await save();
    expect(writesOf("PATCH")[0]?.body).toEqual({ ignoredDevices: [] });
  });

  test("a selector without a serial names the detected device it binds to", async () => {
    const scx = {
      ...brother,
      device: {
        ...brother.device,
        vendorId: "04e8",
        productId: "342e",
        serial: "Z9",
        manufacturer: "Samsung",
        product: "SCX-4300 Series",
      },
      printerId: "scx-4300",
    };
    // Two identical devices without serials must not give two options with one id.
    await mount(status({ devices: [scx, brother, brother] }));
    const entry = await edit("Samsung SCX-4300");
    const usb = button(/USB device/, entry);
    expect(usb.textContent).toContain("Samsung SCX-4300 Series (Z9)");
    expect(usb.textContent).not.toContain("not plugged in");
    await type("Location", "Hall", entry);
    writes.push(() => json(applied("e2")));
    await save();
    expect(writesOf("PATCH")[0]?.body).toEqual({
      printers: [{ ...samsung, location: "Hall" }, hp],
    });
  });

  test("a printer's USB device is picked from the detected devices", async () => {
    await mount(status({ devices: [brother] }));
    const entry = await edit("HP OfficeJet");
    await choose(button(/USB device/, entry), "Brother HL-L2350DW (E7)");
    writes.push(() => json(applied("e2")));
    await save();
    expect(writesOf("PATCH")[0]?.body).toEqual({
      printers: [samsung, { ...hp, usb: { vendorId: "04f9", productId: "0042", serial: "E7" } }],
    });
  });

  test("an entry without a USB selector shows so and saves unchanged", async () => {
    const { usb: _, ...bare } = samsung;
    getConfig = () => json(applied("e1", config({ printers: [bare, hp] })));
    await mount();
    const entry = await edit("Samsung SCX-4300");
    expect(button(/USB device/, entry).textContent).toContain("Any device");
    await type("Location", "Hall", entry);
    writes.push(() => json(applied("e2")));
    await save();
    expect(writesOf("PATCH")[0]?.body).toEqual({ printers: [{ ...bare, location: "Hall" }, hp] });
  });

  test("switching a printer to simulated drops its USB selector and driver name", async () => {
    getConfig = () =>
      json(applied("e1", withPrinter(0, { driver: "legacy", driverName: "samsung-scx" })));
    await mount();
    const entry = await edit("Samsung SCX-4300");
    expect(field("Driver name", entry).value).toBe("samsung-scx");
    act(() => {
      fireEvent.click(within(entry).getByRole("radio", { name: "Simulated printer" }));
    });
    expect(within(entry).queryByRole("textbox", { name: "Driver name" })).toBeNull();
    expect(within(entry).queryByRole("button", { name: /USB device/ })).toBeNull();
    writes.push(() => json(applied("e2")));
    await save();
    const { usb: _, ...rest } = samsung;
    expect(writesOf("PATCH")[0]?.body).toEqual({
      printers: [{ ...rest, mode: "simulated", driver: "legacy" }, hp],
    });
  });

  test("choosing the driverless driver hides and drops the driver name", async () => {
    getConfig = () =>
      json(applied("e1", withPrinter(0, { driver: "legacy", driverName: "samsung-scx" })));
    await mount();
    const entry = await edit("Samsung SCX-4300");
    await choose(button(/Printer driver/, entry), "Driverless (IPP over USB)");
    expect(within(entry).queryByRole("textbox", { name: "Driver name" })).toBeNull();
    writes.push(() => json(applied("e2")));
    await save();
    expect(writesOf("PATCH")[0]?.body).toEqual({
      printers: [{ ...samsung, driver: "ipp-usb" }, hp],
    });
  });

  test("clearing the driver name removes it from the entry", async () => {
    getConfig = () =>
      json(applied("e1", withPrinter(0, { driver: "legacy", driverName: "samsung-scx" })));
    await mount();
    const entry = await edit("Samsung SCX-4300");
    expect(field("Driver name", entry).closest(".ui-field")?.textContent).toContain(
      "leave empty to choose automatically",
    );
    await type("Driver name", "", entry);
    writes.push(() => json(applied("e2")));
    await save();
    expect(writesOf("PATCH")[0]?.body).toEqual({
      printers: [{ ...samsung, driver: "legacy" }, hp],
    });
  });

  test("a scanner's backend is a choice of its own", async () => {
    await mount();
    const entry = await edit("CanoScan LiDE 100");
    await choose(button(/Scanner driver/, entry), "SANE");
    writes.push(() => json(applied("e2")));
    await save();
    expect(writesOf("PATCH")[0]?.body).toEqual({ scanners: [{ ...lide, backend: "airsane" }] });
  });

  test("a 422 puts each error on its field, opening its entry, and lists the rest", async () => {
    await mount();
    await type("Title", "Bad\u0007");
    writes.push(() =>
      problem(422, "about:blank", [
        { path: "/name", line: 2, column: 7, message: "must not contain control characters" },
        { path: "/printers/1/name", line: 9, column: 11, message: "duplicate printer name" },
        { path: "/nonsense", line: 9, column: 1, message: "something else" },
      ]),
    );
    await save();
    await waitFor(() => expect(field("Title").getAttribute("aria-invalid")).toBe("true"));
    expect(document.body.textContent).toContain("must not contain control characters");
    const entry = screen.getByRole("region", { name: "HP OfficeJet" });
    expect(field("Name", entry).getAttribute("aria-invalid")).toBe("true");
    expect(entry.textContent).toContain("duplicate printer name");
    const summary = screen.getByRole("alert");
    expect(summary.textContent).toContain("/nonsense: something else");
    expect(summary.textContent).not.toContain("duplicate printer name");
  });

  test("Reset discards edits", async () => {
    await mount();
    await type("Title", "Office");
    await press(button("Reset"));
    expect(field("Title").value).toBe("Home");
  });

  test("unsaved edits keep Reset and Save in a sticky bar that says so", async () => {
    await mount();
    const bar = () => button("Save").closest(".settings-actions") as HTMLElement;
    expect(bar().hasAttribute("data-sticky")).toBe(false);
    expect(screen.queryByText("Unsaved changes")).toBeNull();
    await type("Title", "Office");
    expect(bar().hasAttribute("data-sticky")).toBe(true);
    expect(within(bar()).getByText("Unsaved changes")).toBeTruthy();
    FakeEventSource.last.emit("config", { etag: "e9" });
    expect(await within(bar()).findByText(/The file changed on disk/)).toBeTruthy();
  });

  test("leaving with unsaved edits asks first, on links and on page unload", async () => {
    window.history.replaceState(null, "", "/settings");
    try {
      await mount();
      const unload = () => {
        const e = new Event("beforeunload", { cancelable: true });
        window.dispatchEvent(e);
        return e.defaultPrevented;
      };
      expect(unload()).toBe(false);
      await type("Title", "Office");
      expect(unload()).toBe(true);

      act(() => navigate("/print"));
      let dialog = await screen.findByRole("alertdialog", { name: "Leave without saving?" });
      expect(window.location.pathname).toBe("/settings");
      await press(button("Keep editing", dialog));
      expect(screen.queryByRole("alertdialog")).toBeNull();
      expect(field("Title").value).toBe("Office");

      act(() => navigate("/print"));
      dialog = await screen.findByRole("alertdialog", { name: "Leave without saving?" });
      await press(button("Leave", dialog));
      expect(window.location.pathname).toBe("/print");
    } finally {
      window.history.replaceState(null, "", "/");
    }
  });
});

describe("defaults", () => {
  const option = (entry: HTMLElement, title: string) =>
    within(entry).getByRole("button", { name: new RegExp(`${title}$`) });

  test("the controls are dropdowns, never radio groups", async () => {
    await mount();
    const entry = await edit("Samsung SCX-4300");
    expect(option(entry, "Sides").getAttribute("aria-haspopup")).toBe("listbox");
    expect(within(entry).queryByRole("radiogroup", { name: "Sides" })).toBeNull();
    const scanner = await edit("CanoScan LiDE 100");
    expect(option(scanner, "Scan from").getAttribute("aria-haspopup")).toBe("listbox");
    expect(within(scanner).queryByRole("radiogroup", { name: "File type" })).toBeNull();
  });

  test("every option kind shows, in the shared groups", async () => {
    await mount();
    const entry = await edit("Samsung SCX-4300");
    for (const name of ["Paper", "Printing", "Layout", "Output"])
      expect(within(entry).getByRole("heading", { level: 4, name })).toBeTruthy();
    expect(entry.querySelectorAll(".settings-option-group [aria-haspopup=listbox]")).toHaveLength(
      13,
    );
  });

  test("the Samsung's sides are disabled with the reason as the accessible description", async () => {
    await mount();
    const entry = await edit("Samsung SCX-4300");
    const sides = option(entry, "Sides");
    expect(isDisabled(sides)).toBe(true);
    await waitFor(() =>
      expect(description(sides)).toContain("This printer only prints one-sided."),
    );
    expect(isDisabled(option(entry, "Quality"))).toBe(false);
  });

  test("the Samsung's paper tray is disabled as not reported", async () => {
    await mount();
    const entry = await edit("Samsung SCX-4300");
    const tray = option(entry, "Paper tray");
    expect(isDisabled(tray)).toBe(true);
    await waitFor(() => expect(description(tray)).toContain("Not reported by this printer."));
  });

  test("the LiDE's source is disabled: it has no document feeder", async () => {
    await mount();
    const entry = await edit("CanoScan LiDE 100");
    const source = option(entry, "Scan from");
    expect(isDisabled(source)).toBe(true);
    await waitFor(() =>
      expect(description(source)).toContain("This scanner has no document feeder."),
    );
  });

  test("a printer that is not connected shows every option disabled with that reason", async () => {
    await mount();
    const entry = await edit("HP OfficeJet");
    const controls = [
      ...entry.querySelectorAll<HTMLElement>(".settings-option-group [aria-haspopup=listbox]"),
    ];
    expect(controls).toHaveLength(13);
    for (const c of controls) {
      expect(isDisabled(c)).toBe(true);
      await waitFor(() =>
        expect(description(c)).toContain(
          "pagewire shows this printer's options once it is connected.",
        ),
      );
    }
  });

  test("a changed default is saved in that entry only", async () => {
    await mount();
    const entry = await edit("Samsung SCX-4300");
    await choose(option(entry, "Quality"), "High");
    writes.push(() => json(applied("e2")));
    await save();
    expect(writesOf("PATCH")[0]?.body).toEqual({
      printers: [{ ...samsung, defaults: { quality: "high" } }, hp],
    });
  });

  test("choosing the printer default removes the key", async () => {
    getConfig = () => json(applied("e1", withPrinter(0, { defaults: { quality: "high" } })));
    await mount();
    const entry = await edit("Samsung SCX-4300");
    await choose(option(entry, "Quality"), "Printer default (Normal)");
    writes.push(() => json(applied("e2")));
    await save();
    expect(writesOf("PATCH")[0]?.body).toEqual({ printers: [samsung, hp] });
  });

  test("saving the Samsung removes a saved default it does not offer", async () => {
    getConfig = () =>
      json(applied("e1", withPrinter(0, { defaults: { mediaSource: "tray-1", quality: "high" } })));
    await mount();
    const entry = await edit("Samsung SCX-4300");
    // Not a change by itself: a clean form stays clean.
    expect((button("Save") as HTMLButtonElement).disabled).toBe(true);
    await choose(option(entry, "Quality"), "Draft");
    writes.push(() => json(applied("e2")));
    await save();
    expect(writesOf("PATCH")[0]?.body).toEqual({
      printers: [{ ...samsung, defaults: { quality: "draft" } }, hp],
    });
  });

  test("other entries go out unchanged, even with defaults their device does not offer", async () => {
    const other = { ...hp, defaults: { mediaSource: "tray-9" } };
    getConfig = () => json(applied("e1", config({ printers: [samsung, other] })));
    await mount(
      status({
        printers: [
          printerStatus(samsung, { capabilities: samsungCaps }),
          printerStatus(hp, { capabilities: { ...noCaps, mediaSources: ["auto", "tray-1"] } }),
        ],
      }),
    );
    const entry = await edit("Samsung SCX-4300");
    await choose(option(entry, "Quality"), "High");
    writes.push(() => json(applied("e2")));
    await save();
    expect(writesOf("PATCH")[0]?.body).toEqual({
      printers: [{ ...samsung, defaults: { quality: "high" } }, other],
    });
  });

  test("a disconnected device keeps its saved defaults on save", async () => {
    const saved = { ...hp, defaults: { mediaSource: "tray-1" } };
    getConfig = () => json(applied("e1", config({ printers: [samsung, saved] })));
    await mount();
    const entry = await edit("HP OfficeJet");
    expect(option(entry, "Paper tray").textContent).toContain("Tray 1");
    await type("Location", "Hall", entry);
    writes.push(() => json(applied("e2")));
    await save();
    expect(writesOf("PATCH")[0]?.body).toEqual({
      printers: [samsung, { ...saved, location: "Hall" }],
    });
  });
});

describe("412 with a valid file", () => {
  const theirs = config({ name: "Kitchen", print: { maxUploadMiB: 80 } });

  async function conflict() {
    await mount();
    await type("Title", "Office");
    writes.push(() => json(applied("e2", theirs), 412));
    await save();
    return screen.findByRole("dialog", { name: "Someone else changed the settings" });
  }

  test("lists each changed field and Keep mine re-sends the patch with the new ETag", async () => {
    const dialog = await conflict();
    const row = within(dialog).getAllByRole("listitem")[0] as HTMLElement;
    expect(row.textContent).toContain("Title");
    expect(row.textContent).toContain("Yours");
    expect(row.textContent).toContain("Office");
    expect(row.textContent).toContain("In the file now");
    expect(row.textContent).toContain("Kitchen");
    // Fields only the other edit changed are listed too.
    const also = within(dialog).getByRole("list", { name: "Also changed in the file" });
    expect(also.textContent).toContain("Upload limit");
    expect(also.textContent).toContain("80");
    expect(also.textContent).not.toContain("Title");

    writes.push(() => json(applied("e3", { ...theirs, name: "Office" })));
    await press(button("Keep mine", dialog));
    const patches = writesOf("PATCH");
    expect(patches).toHaveLength(2);
    expect(patches[1]?.body).toEqual({ name: "Office" });
    expect(patches[1]?.headers["If-Match"]).toBe('"e2"');
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(field("Title").value).toBe("Office");
    expect(field("Upload limit (MB)").value).toBe("80");
  });

  test("a changed device list names the entries on each side", async () => {
    await mount();
    act(() => {
      fireEvent.click(
        within(screen.getByRole("region", { name: "HP OfficeJet" })).getByRole("switch"),
      );
    });
    writes.push(() => json(applied("e2", config({ printers: [samsung] })), 412));
    await save();
    const dialog = await screen.findByRole("dialog", { name: "Someone else changed the settings" });
    const row = within(dialog).getAllByRole("listitem")[0] as HTMLElement;
    expect(row.textContent).toContain("Printers");
    expect(row.textContent).toContain("HP OfficeJet (off)");
    expect(row.textContent).not.toContain("undefined");
  });

  test("Use the file loads the file's values", async () => {
    const dialog = await conflict();
    await press(button("Use the file", dialog));
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(field("Title").value).toBe("Kitchen");
    expect((button("Save") as HTMLButtonElement).disabled).toBe(true);
  });

  test("Show the file's values loads them and marks your edits", async () => {
    const dialog = await conflict();
    await press(button("Show the file's values", dialog));
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(field("Title").value).toBe("Kitchen");
    expect(document.body.textContent).toContain("Title: Office");
    expect(field("Title").closest(".settings-highlight")).not.toBeNull();
    expect(field("Upload limit (MB)").closest(".settings-highlight")).toBeNull();
  });

  test("a second 412 on Keep mine shows the newer file", async () => {
    const dialog = await conflict();
    writes.push(() => json(applied("e3", config({ name: "Garage" })), 412));
    await press(button("Keep mine", dialog));
    const again = await screen.findByRole("dialog", { name: "Someone else changed the settings" });
    expect(within(again).getByRole("listitem").textContent).toContain("Garage");
  });
});

describe("412 with an invalid file", () => {
  async function conflict() {
    await mount();
    await type("Title", "Office");
    writes.push(() => json(invalid("e2"), 412));
    await save();
    return screen.findByRole("dialog", { name: "The file on disk changed and is now invalid" });
  }

  test("shows the errors and replaces the file with a full PUT after a warning", async () => {
    const dialog = await conflict();
    expect(dialog.textContent).toContain("Line 3, column 1");
    expect(dialog.textContent).toContain("mapping values are not allowed here");

    await press(button("Replace the file with my settings", dialog));
    expect(dialog.textContent).toContain("comments");
    expect(writesOf("PUT")).toHaveLength(0);

    writes.push(() => json(applied("e3", config({ name: "Office" }))));
    await press(button("Replace the file", dialog));
    const [put] = writesOf("PUT");
    expect(put?.body).toEqual(config({ name: "Office" }));
    expect(put?.headers["If-Match"]).toBe('"e2"');
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(field("Title").value).toBe("Office");
  });

  test("Cancel closes the dialog and shows the invalid-file view", async () => {
    const dialog = await conflict();
    await press(button("Cancel", dialog));
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(screen.getByRole("heading", { name: "The settings file has errors" })).toBeTruthy();
    expect(writesOf("PUT")).toHaveLength(0);
  });
});

describe("file changes on disk", () => {
  test("a config event while editing shows a banner and keeps the edit", async () => {
    await mount();
    await type("Title", "Office");
    const gets = writesOf("GET").length;
    FakeEventSource.last.emit("config", { etag: "e9" });
    expect(await screen.findByText(/The file changed on disk/)).toBeTruthy();
    expect(field("Title").value).toBe("Office");
    expect(writesOf("GET").length).toBe(gets);
  });

  test("a config event on a clean form reloads silently", async () => {
    await mount();
    await screen.findByRole("textbox", { name: "Title" });
    getConfig = () => json(applied("e9", config({ name: "Kitchen" })));
    FakeEventSource.last.emit("config", { etag: "e9" });
    await waitFor(() => expect(field("Title").value).toBe("Kitchen"));
    expect(screen.queryByText(/The file changed on disk/)).toBeNull();
  });

  test("the banner does not come back after the conflict is resolved", async () => {
    await mount();
    await type("Title", "Office");
    FakeEventSource.last.emit("config", { etag: "e9" });
    expect(await screen.findByText(/The file changed on disk/)).toBeTruthy();
    writes.push(() => json(applied("e9", config({ print: { maxUploadMiB: 80 } })), 412));
    await save();
    const dialog = await screen.findByRole("dialog", { name: "Someone else changed the settings" });
    writes.push(() =>
      json(applied("e10", config({ name: "Office", print: { maxUploadMiB: 80 } }))),
    );
    await press(button("Keep mine", dialog));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    await type("Title", "Office 2");
    expect(screen.queryByText(/The file changed on disk/)).toBeNull();
  });

  test("a reload that lands during a new edit keeps the edit and the old ETag", async () => {
    await mount();
    await screen.findByRole("textbox", { name: "Title" });
    let resolve: (r: Response) => void = () => {};
    getConfig = () =>
      new Promise<Response>((r) => {
        resolve = r;
      }) as unknown as Response;
    FakeEventSource.last.emit("config", { etag: "e9" });
    await type("Title", "Office");
    await act(async () => {
      resolve(json(applied("e9", config({ print: { maxUploadMiB: 80 } }))));
    });
    expect(await screen.findByText(/The file changed on disk/)).toBeTruthy();
    expect(field("Title").value).toBe("Office");
    expect(field("Upload limit (MB)").value).toBe("50");
    writes.push(() => json(applied("e9", config({ print: { maxUploadMiB: 80 } })), 412));
    await save();
    expect(writesOf("PATCH")[0]?.headers["If-Match"]).toBe('"e1"');
  });

  test("the echo of our own save is ignored", async () => {
    await mount();
    await screen.findByRole("textbox", { name: "Title" });
    const gets = writesOf("GET").length;
    FakeEventSource.last.emit("config", { etag: "e1" });
    expect(writesOf("GET").length).toBe(gets);
  });
});

describe("invalid file", () => {
  test("shows the errors and a download link, without restore when there is no last good", async () => {
    getConfig = () => json(invalid("e5"));
    await mount();
    expect(
      await screen.findByRole("heading", { name: "The settings file has errors" }),
    ).toBeTruthy();
    expect(document.body.textContent).toContain("Line 3, column 1");
    const link = screen.getByRole("link", { name: "Download the file" });
    expect(link.getAttribute("href")).toBe("/api/v1/config/raw");
    expect(screen.queryByRole("button", { name: "Restore last working settings" })).toBeNull();
  });

  test("restores the last good config with a PUT after confirmation", async () => {
    const lastGood = config({ name: "Old" });
    getConfig = () => json(invalid("e5", lastGood));
    await mount();
    await press(await screen.findByRole("button", { name: "Restore last working settings" }));
    const confirm = screen.getByRole("alertdialog", { name: "Restore the last working settings?" });
    expect(writesOf("PUT")).toHaveLength(0);
    writes.push(() => json(applied("e6", lastGood)));
    await press(button("Restore", confirm));
    const [put] = writesOf("PUT");
    expect(put?.body).toEqual(lastGood);
    expect(put?.headers["If-Match"]).toBe('"e5"');
    await waitFor(() => expect(field("Title").value).toBe("Old"));
  });

  test("a 412 on restore shows the file on disk now", async () => {
    getConfig = () => json(invalid("e5", config({ name: "Old" })));
    await mount();
    await press(await screen.findByRole("button", { name: "Restore last working settings" }));
    const confirm = screen.getByRole("alertdialog", { name: "Restore the last working settings?" });
    writes.push(() => json(applied("e7", config({ name: "Fixed by hand" })), 412));
    await press(button("Restore", confirm));
    await waitFor(() => expect(field("Title").value).toBe("Fixed by hand"));
    expect(screen.queryByRole("dialog")).toBeNull();
  });
});

describe("licences", () => {
  test("shows the blob state and revoking sends the new list", async () => {
    await mount(
      status({ blobs: [{ ...HPLIP, state: "failed", message: "HP plug-in has no arm64 build" }] }),
    );
    expect(await screen.findByText("Download failed: HP plug-in has no arm64 build")).toBeTruthy();
    const box = screen.getByRole("checkbox", {
      name: /I accept the HP .* licence shown above/,
    }) as HTMLInputElement;
    expect(box.checked).toBe(true);
    act(() => {
      fireEvent.click(box);
    });
    writes.push(() => json(applied("e2", config({ licenses: { acceptedEulas: [] } }))));
    await save();
    expect(writesOf("PATCH")[0]?.body).toEqual({ licenses: { acceptedEulas: [] } });
  });

  test("accepting sends the EULA id, version and the sha256 of the text shown", async () => {
    getConfig = () => json(applied("e1", config({ licenses: { acceptedEulas: [] } })));
    await mount();
    await screen.findByRole("region", { name: /licence text/ });
    const box = screen.getByRole("checkbox", {
      name: /I accept the HP .* licence shown above/,
    }) as HTMLInputElement;
    expect(box.checked).toBe(false);
    act(() => {
      fireEvent.click(box);
    });
    writes.push(() => json(applied("e2")));
    await save();
    expect(writesOf("PATCH")[0]?.body).toEqual({ licenses: { acceptedEulas: [HPLIP_ACCEPTED] } });
  });

  test("a changed licence text asks again and accepting replaces the old entry", async () => {
    eulaSha = "d".repeat(64);
    await mount();
    await screen.findByText(/licence text has changed since you accepted it/);
    const box = screen.getByRole("checkbox", {
      name: /I accept the HP .* licence shown above/,
    }) as HTMLInputElement;
    expect(box.checked).toBe(false);
    act(() => {
      fireEvent.click(box);
    });
    writes.push(() => json(applied("e2")));
    await save();
    expect(writesOf("PATCH")[0]?.body).toEqual({
      licenses: { acceptedEulas: [{ ...HPLIP, textSha256: eulaSha }] },
    });
  });
});

test("a missing config points to setup", async () => {
  getConfig = () => problem(404, "config-missing");
  await mount(null);
  expect(await screen.findByText("pagewire is not set up yet")).toBeTruthy();
  expect(screen.getByRole("link", { name: "Go to setup" }).getAttribute("href")).toBe("/setup");
});
