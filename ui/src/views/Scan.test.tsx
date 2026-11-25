import { afterEach, beforeEach, describe, expect, spyOn, test } from "bun:test";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { createEventStore, EventsContext } from "../api/events";
import { toastQueue } from "../components/ui";
import type { Config, ConfigResponse, Scan as ScanDTO, ScannerCaps, Status } from "../gen/api";
import { Scan } from "./Scan";

type Call = { method: string; path: string; body?: unknown };
let calls: Call[] = [];
let scans: ScanDTO[] = [];
let caps: ScannerCaps;
let configResponse: ConfigResponse | null = null;
let postScan: () => Response;
let scannerFailures = 0;
let listFailures = 0;

const json = (body: unknown, status = 200) =>
  new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
const problem = (status: number, title: string) =>
  json({ type: "about:blank", title, status, detail: "", errors: [] }, status);

const realFetch = globalThis.fetch;
const fakeFetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
  const path = new URL(String(input), "http://localhost").pathname;
  const method = init?.method ?? "GET";
  calls.push({
    method,
    path,
    body: typeof init?.body === "string" ? JSON.parse(init.body) : undefined,
  });
  if (path === "/api/v1/scanner") {
    if (scannerFailures > 0) {
      scannerFailures--;
      return problem(503, "Service Unavailable");
    }
    return json(caps);
  }
  if (path === "/api/v1/scans" && method === "GET") {
    if (listFailures > 0) {
      listFailures--;
      return problem(500, "Internal Server Error");
    }
    return json(scans);
  }
  if (path === "/api/v1/scans" && method === "POST") return postScan();
  if (path.startsWith("/api/v1/scans/") && method === "DELETE") {
    // A running scan is canceled (202 with its body), a finished one deleted (204).
    const s = scans.find((x) => path.endsWith(x.id));
    return s?.state === "scanning" ? json(s, 202) : new Response(null, { status: 204 });
  }
  if (path === "/api/v1/config") {
    return configResponse ? json(configResponse) : problem(404, "Not found");
  }
  return problem(404, "Not found");
}) as unknown as typeof fetch;

let consoleError: ReturnType<typeof spyOn>;
beforeEach(() => {
  calls = [];
  scans = [];
  configResponse = null;
  scannerFailures = 0;
  listFailures = 0;
  caps = {
    sources: ["platen", "adf", "adf-duplex"],
    resolutions: [75, 150, 300, 600],
    colorModes: ["color", "grayscale", "blackandwhite"],
    adf: true,
    duplex: true,
  };
  postScan = () => json(scan({ state: "scanning", pages: 0 }), 202);
  globalThis.fetch = fakeFetch;
  consoleError = spyOn(console, "error");
});
afterEach(() => {
  // React reports duplicate keys and invalid markup through console.error.
  expect(consoleError).not.toHaveBeenCalled();
  consoleError.mockRestore();
  cleanup();
  globalThis.fetch = realFetch;
});

const ID = "0123456789abcdef";
const OLD = "fedcba9876543210";

function scan(patch: Partial<ScanDTO> = {}): ScanDTO {
  return {
    id: ID,
    state: "completed",
    createdAt: "2026-11-24T17:00:00+01:00",
    pages: 1,
    format: "pdf",
    settings: { source: "platen", resolution: 300, colorMode: "color", format: "pdf" },
    error: null,
    ...patch,
  };
}

const noScanner: Status = {
  config: {
    state: "missing",
    etag: "",
    appliedEtag: "",
    errors: [],
    changedAt: "0001-01-01T00:00:00Z",
  },
  device: {
    phase: "none",
    mode: "none",
    decision: null,
    inventory: null,
    message: "",
    advertised: [],
    blobs: [],
  },
  printer: {
    available: false,
    uri: "",
    state: "unknown",
    reasons: [],
    formats: [],
    convertedFormats: [],
    media: [],
    markers: [],
  },
  scanner: { available: false, url: "", state: "unknown" },
  reconcile: [],
  connect: { dnssdName: "", ippUris: [], esclUrls: [] },
};

const ready: Status = {
  ...noScanner,
  config: { ...noScanner.config, state: "applied", etag: "e1", appliedEtag: "e1" },
  device: { ...noScanner.device, phase: "ready", mode: "simulated" },
  scanner: { available: true, url: "http://127.0.0.1:8090/eSCL", state: "idle" },
};

const config: Config = {
  apiVersion: "pagewire/v1",
  name: "Living room printer",
  location: "",
  device: { mode: "simulated", driver: "auto", scanner: "auto" },
  print: {
    defaults: {
      media: "iso_a4_210x297mm",
      sides: "one-sided",
      colorMode: "auto",
      quality: "normal",
    },
    maxUploadMiB: 50,
  },
  scan: {
    defaults: { source: "adf-duplex", resolution: 300, colorMode: "color", format: "jpeg" },
    retentionDays: 3,
  },
  licenses: { acceptedEulas: [] },
};

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

const emit = (type: string, data: unknown) =>
  act(async () => FakeEventSource.last?.emit(type, data));

async function renderScan(status: Status | null, online = true) {
  const store = createEventStore({
    newEventSource: () => new FakeEventSource() as unknown as EventSource,
    minBackoffMs: 0,
    notFoundAfterMs: 20,
  });
  store.start();
  if (online) act(() => FakeEventSource.last?.onopen?.(new Event("open")));
  if (status) act(() => FakeEventSource.last?.emit("status", status));
  let view!: ReturnType<typeof render>;
  await act(async () => {
    view = render(
      <EventsContext value={store}>
        <Scan />
      </EventsContext>,
    );
  });
  return view;
}

const scanButton = () => screen.getByRole("button", { name: "Scan" });
const disabled = (el: HTMLElement) => el.hasAttribute("disabled");
/** The visible value of the Select labelled `label`. */
const fieldOf = (label: string) =>
  screen
    .getAllByText(label)
    .find((e) => e.classList.contains("ui-label"))
    ?.closest(".ui-field") as HTMLElement;
const selected = (label: string) => fieldOf(label)?.querySelector(".ui-select-value")?.textContent;
/** The options of a Select, read from React Aria's hidden native select. */
const optionsOf = (label: string) =>
  Array.from(fieldOf(label)?.querySelectorAll("select option") ?? [])
    .map((o) => o.textContent?.trim())
    .filter(Boolean);
const press = (el: HTMLElement) =>
  act(async () => {
    fireEvent.click(el);
  });

describe("Scan view", () => {
  test("renders before the first Status arrives", async () => {
    await renderScan(null);
    expect(screen.getByRole("heading", { level: 1, name: "Scan" })).toBeTruthy();
    expect(screen.getByText("Checking the scanner…")).toBeTruthy();
  });

  test("without a scanner the Scan button is disabled and capabilities are not asked", async () => {
    await renderScan({
      ...noScanner,
      device: { ...noScanner.device, phase: "probing", message: "No printer found on USB" },
    });
    expect(screen.getByText("The scanner is not ready")).toBeTruthy();
    expect(screen.getByText("No printer found on USB")).toBeTruthy();
    expect(disabled(scanButton())).toBe(true);
    // GET /scanner answers 503 and GET /config 404 in this state, so neither is asked.
    expect(calls.some((c) => c.path === "/api/v1/scanner")).toBe(false);
    expect(calls.some((c) => c.path === "/api/v1/config")).toBe(false);
  });

  test("options are limited to the scanner's capabilities and default from the config", async () => {
    configResponse = { etag: "e1", config, state: "applied", errors: [], lastGood: null };
    caps = {
      sources: ["platen", "adf"],
      resolutions: [100, 200, 600],
      colorModes: ["grayscale", "blackandwhite"],
      adf: true,
      duplex: false,
    };
    await renderScan(ready);
    // adf-duplex is not offered, so the first source is used; 300 dpi falls back to the nearest.
    await waitFor(() => expect(selected("Resolution")).toBe("200 dpi"));
    expect(selected("Source")).toBe("Glass");
    expect(selected("Colour")).toBe("Greyscale");
    expect(optionsOf("Source")).toEqual(["Glass", "Document feeder"]);
    expect(optionsOf("Resolution")).toEqual(["100 dpi", "200 dpi", "600 dpi"]);
    expect(optionsOf("Colour")).toEqual(["Greyscale", "Black and white"]);
    expect((screen.getByRole("radio", { name: /^JPEG/ }) as HTMLInputElement).checked).toBeTruthy();
    expect(screen.getByText("Scans are deleted after 3 days.")).toBeTruthy();

    await press(scanButton());
    expect(calls.find((c) => c.method === "POST")?.body).toEqual({
      source: "platen",
      resolution: 200,
      colorMode: "grayscale",
      format: "jpeg",
    });
  });

  test("a scan shows live pages as thumbnails and ends with a download", async () => {
    postScan = () =>
      json(
        scan({
          state: "scanning",
          pages: 0,
          format: "jpeg",
          settings: { ...scan().settings, format: "jpeg" },
        }),
        202,
      );
    await renderScan(ready);
    await waitFor(() => expect(disabled(scanButton())).toBe(false));
    await press(scanButton());
    expect(screen.getByRole("progressbar", { name: "Scanning…" })).toBeTruthy();
    // Pending, not disabled, so keyboard focus stays on the button.
    expect(disabled(scanButton())).toBe(false);
    expect(scanButton().hasAttribute("data-pending")).toBe(true);
    // No page count before the first page arrives.
    expect(document.body.textContent).not.toContain("0 pages");

    const live = { state: "scanning" as const, format: "jpeg" as const };
    await emit("scan", scan({ ...live, pages: 1 }));
    expect(screen.getByRole("progressbar", { name: "Scanning… 1 page so far" })).toBeTruthy();
    const first = screen.getByAltText("Page 1") as HTMLImageElement;
    expect(first.getAttribute("loading")).toBe("lazy");
    expect(first.getAttribute("src")).toBe(`/api/v1/scans/${ID}/pages/1`);
    // A running scan cannot be downloaded or deleted yet (the server answers 409).
    expect(screen.queryByRole("link", { name: /^Download/ })).toBeNull();
    expect(screen.queryByRole("button", { name: /^Delete scan from/ })).toBeNull();

    await emit("scan", scan({ ...live, pages: 2 }));
    expect(screen.getByRole("progressbar", { name: "Scanning… 2 pages so far" })).toBeTruthy();
    expect(screen.getByAltText("Page 2")).toBeTruthy();

    await emit("scan", scan({ ...live, state: "completed", pages: 2 }));
    expect(screen.queryByRole("progressbar")).toBeNull();
    const link = screen.getByRole("link", { name: "Download ZIP" });
    expect(link.getAttribute("href")).toBe(`/api/v1/scans/${ID}/download`);
    expect(link.hasAttribute("download")).toBe(true);
    expect(disabled(scanButton())).toBe(false);
    expect(scanButton().hasAttribute("data-pending")).toBe(false);
    expect(toastQueue.visibleToasts.map((t) => t.content.title)).toEqual(["Scan ready, 2 pages"]);
  });

  test("the download names the file type", async () => {
    scans = [scan({ format: "pdf", pages: 3 })];
    const view = await renderScan(ready);
    await waitFor(() => expect(screen.getByRole("link", { name: "Download PDF" })).toBeTruthy());
    cleanup();
    view.unmount();

    scans = [scan({ format: "jpeg", pages: 1 })];
    await renderScan(ready);
    await waitFor(() => expect(screen.getByRole("link", { name: "Download JPG" })).toBeTruthy());
  });

  test("a thumbnail opens the full-size page in a dialog", async () => {
    scans = [scan({ pages: 2 })];
    await renderScan(ready);
    const open = await screen.findByRole("button", { name: "Open page 2" });
    await press(open);
    const dialog = screen.getByRole("dialog", { name: "Page 2 of 2" });
    const img = within(dialog).getByAltText("Page 2") as HTMLImageElement;
    expect(img.getAttribute("src")).toBe(`/api/v1/scans/${ID}/pages/2`);
  });

  test("history lists scans, shows another one, and deletes after confirmation", async () => {
    scans = [
      scan({ pages: 1 }),
      scan({ id: OLD, createdAt: "2026-11-23T09:00:00Z", pages: 2, format: "jpeg" }),
    ];
    await renderScan(ready);
    const list = await screen.findByRole("list", { name: "Recent scans" });
    await waitFor(() => expect(within(list).getAllByRole("listitem").length).toBe(2));
    expect(screen.getByRole("link", { name: "Download PDF" })).toBeTruthy();

    const rows = within(list).getAllByRole("listitem");
    await press(within(rows[1] as HTMLElement).getByRole("button", { name: /^Show scan from/ }));
    expect(screen.getByRole("link", { name: "Download ZIP" })).toBeTruthy();

    const del = () => within(rows[1] as HTMLElement).getByRole("button", { name: /^Delete/ });
    await press(del());
    let confirm = screen.getByRole("alertdialog", { name: "Delete this scan?" });
    await press(within(confirm).getByRole("button", { name: "Cancel" }));
    expect(calls.some((c) => c.method === "DELETE")).toBe(false);
    expect(within(list).getAllByRole("listitem").length).toBe(2);

    await press(del());
    confirm = screen.getByRole("alertdialog", { name: "Delete this scan?" });
    await press(within(confirm).getByRole("button", { name: "Delete scan" }));
    expect(calls).toContainEqual({ method: "DELETE", path: `/api/v1/scans/${OLD}` });
    await waitFor(() => expect(within(list).getAllByRole("listitem").length).toBe(1));
    expect(screen.queryByRole("alertdialog")).toBeNull();
  });

  test("a busy scanner answers 409 and the page says so", async () => {
    postScan = () => problem(409, "Conflict");
    await renderScan(ready);
    await waitFor(() => expect(disabled(scanButton())).toBe(false));
    await press(scanButton());
    expect(
      screen.getByText("The scanner is busy. Wait for the current scan to finish."),
    ).toBeTruthy();
    expect(disabled(scanButton())).toBe(false);
  });

  test("a scanner that is processing shows the Scan button pending", async () => {
    await renderScan({ ...ready, scanner: { ...ready.scanner, state: "processing" } });
    // Options showing means the capabilities arrived, so only the processing state holds Scan.
    await waitFor(() => expect(selected("Resolution")).toBe("300 dpi"));
    expect(scanButton().hasAttribute("data-pending")).toBe(true);
  });

  test("scanning is off while the connection to pagewire is down", async () => {
    await renderScan(ready, false);
    await waitFor(() => expect(selected("Resolution")).toBe("300 dpi"));
    expect(disabled(scanButton())).toBe(true);
  });

  test("a running scan can be canceled and stays listed as canceled", async () => {
    scans = [scan({ state: "scanning", pages: 1 })];
    await renderScan(ready);
    const cancel = await screen.findByRole("button", { name: "Cancel scan" });
    await press(cancel);
    expect(calls).toContainEqual({ method: "DELETE", path: `/api/v1/scans/${ID}` });
    const list = screen.getByRole("list", { name: "Recent scans" });
    expect(within(list).getAllByRole("listitem").length).toBe(1);
    await emit("scan", scan({ state: "canceled", pages: 0 }));
    expect(screen.getByText("The scan was canceled.")).toBeTruthy();
    expect(within(list).getByText("Canceled")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Cancel scan" })).toBeNull();
    // Canceling is not announced as a failure.
    expect(toastQueue.visibleToasts.length).toBe(0);
  });

  test("a scan that fails while you watch is announced", async () => {
    scans = [scan({ state: "scanning", pages: 0 })];
    await renderScan(ready);
    await screen.findByRole("button", { name: "Cancel scan" });
    await emit("scan", scan({ state: "failed", pages: 0, error: "paper jam" }));
    expect(toastQueue.visibleToasts.map((t) => t.content.title)).toEqual(["Scan failed"]);
  });

  test("an empty history says so, and a failed list can be retried", async () => {
    listFailures = 1;
    await renderScan(ready);
    await press(await screen.findByRole("button", { name: "Try again" }));
    expect(await screen.findByText("No recent scans")).toBeTruthy();
    expect(screen.queryByText("Could not load the scan list.")).toBeNull();
  });

  test("the page preview steps to the previous and next page", async () => {
    scans = [scan({ pages: 3 })];
    await renderScan(ready);
    await press(await screen.findByRole("button", { name: "Open page 1" }));
    let dialog = screen.getByRole("dialog", { name: "Page 1 of 3" });
    expect(disabled(within(dialog).getByRole("button", { name: "Previous page" }))).toBe(true);
    await press(within(dialog).getByRole("button", { name: "Next page" }));
    dialog = screen.getByRole("dialog", { name: "Page 2 of 3" });
    expect(within(dialog).getByAltText("Page 2")).toBeTruthy();
    await press(within(dialog).getByRole("button", { name: "Next page" }));
    dialog = screen.getByRole("dialog", { name: "Page 3 of 3" });
    expect(disabled(within(dialog).getByRole("button", { name: "Next page" }))).toBe(true);
    await press(within(dialog).getByRole("button", { name: "Previous page" }));
    expect(screen.getByRole("dialog", { name: "Page 2 of 3" })).toBeTruthy();
  });

  test("full scan storage asks you to delete old scans", async () => {
    postScan = () =>
      json(
        {
          type: "storage-full",
          title: "Insufficient Storage",
          status: 507,
          detail: "stored scans use 1 GiB; delete old scans to scan again",
          errors: [],
        },
        507,
      );
    await renderScan(ready);
    await waitFor(() => expect(disabled(scanButton())).toBe(false));
    await press(scanButton());
    expect(screen.getByText(/Delete old scans under Recent scans/)).toBeTruthy();
  });

  test("a device that finds no printer tells you to plug it in", async () => {
    await renderScan({
      ...noScanner,
      config: { ...noScanner.config, state: "applied" },
      device: {
        ...noScanner.device,
        phase: "probing",
        mode: "probe",
        inventory: { devices: [], probedAt: "2026-11-24T18:00:00Z", simulated: false },
      },
    });
    expect(await screen.findByText(/Plug the printer in with USB/)).toBeTruthy();
  });

  test("a failed capabilities request can be retried", async () => {
    scannerFailures = 1;
    await renderScan(ready);
    await press(await screen.findByRole("button", { name: "Try again" }));
    await waitFor(() => expect(selected("Resolution")).toBe("300 dpi"));
    expect(screen.queryByRole("button", { name: "Try again" })).toBeNull();
    expect(disabled(scanButton())).toBe(false);
  });

  test("a failed scan shows its error", async () => {
    scans = [scan({ state: "failed", pages: 0, error: "the scanner returned no pages" })];
    await renderScan(ready);
    expect(await screen.findByText("the scanner returned no pages")).toBeTruthy();
    expect(screen.getByText("The scan failed")).toBeTruthy();
    expect(screen.queryByRole("link", { name: /^Download/ })).toBeNull();
  });

  test("the list is fetched again after the event stream reconnects", async () => {
    // Not yet connected: the first open below is the first connect, not a reconnect.
    await renderScan(ready, false);
    const lists = () => calls.filter((c) => c.method === "GET" && c.path === "/api/v1/scans");
    await waitFor(() => expect(lists().length).toBe(1));
    scans = [scan()];
    const es = FakeEventSource.last;
    await act(async () => {
      es?.onopen?.(new Event("open"));
      es?.onerror?.(new Event("error"));
      await new Promise((r) => setTimeout(r, 5));
      FakeEventSource.last?.onopen?.(new Event("open"));
    });
    await waitFor(() => expect(lists().length).toBe(2));
    expect(await screen.findByRole("link", { name: "Download PDF" })).toBeTruthy();
  });
});
