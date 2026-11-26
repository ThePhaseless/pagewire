import { afterEach, beforeEach, describe, expect, spyOn, test } from "bun:test";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { ProblemError, type uploadPrint } from "../api/client";
import { createEventStore, EventsContext } from "../api/events";
import { toastQueue } from "../components/ui";
import type {
  Config,
  ConfigResponse,
  Job,
  PrinterCaps,
  PrintOptions,
  PrintResult,
  Status,
  StatusPrinter,
  Warning,
} from "../gen/api";
import { detectFormat, formatAccepted, Print } from "./Print";

type Call = { method: string; path: string };
let calls: Call[] = [];
let jobs: Job[] = [];
let configResponse: ConfigResponse | null = null;

const realFetch = globalThis.fetch;
const fakeFetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
  const path = new URL(String(input), "http://localhost").pathname;
  const method = init?.method ?? "GET";
  calls.push({ method, path });
  const json = (body: unknown, status = 200) =>
    new Response(JSON.stringify(body), {
      status,
      headers: { "Content-Type": "application/json" },
    });
  const jobsOf = /^\/api\/v1\/printers\/([^/]+)\/jobs$/.exec(path);
  if (jobsOf && method === "GET") return json(jobs.filter((j) => j.printer === jobsOf[1]));
  if (/^\/api\/v1\/printers\/[^/]+\/jobs\/\d+$/.test(path) && method === "DELETE")
    return new Response(null, { status: 204 });
  if (path === "/api/v1/config") {
    return configResponse
      ? json(configResponse)
      : json(
          { type: "config-missing", title: "Not found", status: 404, detail: "", errors: [] },
          404,
        );
  }
  return json(
    { type: "about:blank", title: "Not found", status: 404, detail: "", errors: [] },
    404,
  );
}) as unknown as typeof fetch;

let consoleError: ReturnType<typeof spyOn>;
beforeEach(() => {
  calls = [];
  jobs = [];
  configResponse = null;
  localStorage.clear();
  window.history.replaceState(null, "", "/print");
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

/** The Samsung SCX-4300's queue: one-sided, monochrome, and nothing else beyond copies. */
const samsungCaps: PrinterCaps = {
  ...noCaps,
  media: ["iso_a4_210x297mm", "na_letter_8.5x11in"],
  mediaDefault: "na_letter_8.5x11in",
  sides: ["one-sided"],
  sidesDefault: "one-sided",
  colorModes: ["monochrome"],
  colorModeDefault: "monochrome",
  qualities: ["draft", "normal", "high"],
  qualityDefault: "normal",
  resolutions: [300, 600],
  resolutionDefault: 600,
  copiesMax: 999,
};

/** The simulated HP OfficeJet: every kind, with a single output bin and finishing. */
const hpCaps: PrinterCaps = {
  ...noCaps,
  media: [
    "iso_a4_210x297mm",
    "na_letter_8.5x11in",
    "na_legal_8.5x14in",
    "na_number-10_4.125x9.5in",
    "na_index-4x6_4x6in",
  ],
  mediaDefault: "iso_a4_210x297mm",
  mediaSources: ["auto", "tray-1", "manual"],
  mediaSourceDefault: "auto",
  mediaTypes: ["stationery", "photographic-glossy"],
  mediaTypeDefault: "stationery",
  sides: ["one-sided", "two-sided-long-edge", "two-sided-short-edge"],
  sidesDefault: "one-sided",
  colorModes: ["auto", "color", "monochrome"],
  colorModeDefault: "auto",
  qualities: ["draft", "normal", "high"],
  qualityDefault: "normal",
  resolutions: [300, 600],
  resolutionDefault: 600,
  contentOptimize: ["auto", "photo", "text"],
  contentOptimizeDefault: "auto",
  orientations: ["portrait", "landscape"],
  orientationDefault: "portrait",
  numberUp: [1, 2, 4],
  numberUpDefault: 1,
  printScalings: ["auto", "fit", "fill", "none"],
  printScalingDefault: "auto",
  outputBins: ["face-down"],
  outputBinDefault: "face-down",
  finishings: ["none"],
  finishingsDefault: "none",
  copiesMax: 99,
};

function printer(p: Partial<StatusPrinter> = {}): StatusPrinter {
  return {
    id: "hp",
    name: "HP OfficeJet",
    location: "Study",
    enabled: true,
    phase: "ready",
    message: "",
    warnings: [],
    decision: null,
    device: null,
    available: true,
    uri: "ipp://127.0.0.1:60000/ipp/print",
    state: "idle",
    reasons: [],
    // octet-stream never makes an unknown file printable (internal/printing.Negotiate).
    formats: ["application/pdf", "image/jpeg; charset=binary", "application/octet-stream"],
    convertedFormats: [],
    media: [],
    markers: [],
    capabilities: hpCaps,
    connect: { dnssdName: "", ippUris: [] },
    ...p,
  };
}

const samsung = printer({
  id: "samsung",
  name: "Samsung SCX-4300",
  location: "",
  capabilities: samsungCaps,
});

const base: Status = {
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
};

const ready: Status = { ...base, printers: [printer()] };
const both: Status = { ...base, printers: [printer(), samsung] };

const config: Config = {
  apiVersion: "pagewire/v2",
  name: "Home",
  printers: [
    {
      id: "hp",
      name: "HP OfficeJet",
      location: "Study",
      enabled: true,
      mode: "simulated",
      driver: "auto",
      defaults: {
        media: "na_letter_8.5x11in",
        sides: "two-sided-long-edge",
        colorMode: "monochrome",
        quality: "high",
      },
    },
  ],
  scanners: [],
  ignoredDevices: [],
  print: { maxUploadMiB: 1 },
  scan: { retentionDays: 7 },
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

const emit = (type: string, data: unknown) => act(() => FakeEventSource.last?.emit(type, data));

type Upload = {
  printer: string;
  file: File;
  options: PrintOptions;
  progress: (f: number) => void;
  done: (warnings?: Warning[]) => void;
  fail: (e: unknown) => void;
};

/** A controllable upload: each call waits until the test calls done() or fail(). */
function fakeUpload() {
  const uploads: Upload[] = [];
  const upload = ((
    printerId: string,
    file: File,
    options: PrintOptions,
    onProgress?: (f: number) => void,
  ) =>
    new Promise<PrintResult>((resolve, reject) => {
      uploads.push({
        printer: printerId,
        file,
        options,
        progress: (f) => onProgress?.(f),
        done: (warnings = []) =>
          resolve({
            job: {
              id: 1,
              printer: printerId,
              name: file.name,
              state: "pending",
              reasons: [],
              createdAt: "2026-11-24T17:00:00Z",
            },
            warnings,
          }),
        fail: reject,
      });
    })) as unknown as typeof uploadPrint;
  return { upload, uploads };
}

async function renderPrint(status: Status | null, upload?: typeof uploadPrint, online = true) {
  const store = createEventStore({
    newEventSource: () => new FakeEventSource() as unknown as EventSource,
    notFoundAfterMs: 20,
  });
  store.start();
  if (online) act(() => FakeEventSource.last?.onopen?.(new Event("open")));
  if (status) act(() => FakeEventSource.last?.emit("status", status));
  let view!: ReturnType<typeof render>;
  await act(async () => {
    view = render(
      <EventsContext value={store}>
        <Print upload={upload} />
      </EventsContext>,
    );
  });
  return view;
}

const bytes = (s: string) => Uint8Array.from(s, (c) => c.charCodeAt(0));

async function pick(container: HTMLElement, files: File[]) {
  const input = container.querySelector('input[type="file"]') as HTMLInputElement;
  Object.defineProperty(input, "files", { value: files, configurable: true });
  await act(async () => {
    fireEvent.change(input);
  });
}

const row = (name: string) => screen.getByText(name).closest("li") as HTMLElement;
const printButton = () => screen.getByRole("button", { name: /^Print( |$)/ });
const problem = (status: number, type: string, detail: string) =>
  new ProblemError({ type, title: "x", status, detail, errors: [] });
/** The field of the control labelled `label`, or undefined when it is not shown. */
const fieldOf = (label: string) =>
  screen
    .queryAllByText(label)
    .find((e) => e.classList.contains("ui-label"))
    ?.closest(".ui-field") as HTMLElement | undefined;
/** The visible value of the Select labelled `label` (the hidden native select lists every option). */
const selected = (label: string) => fieldOf(label)?.querySelector(".ui-select-value")?.textContent;
const optionsOf = (label: string) =>
  Array.from(fieldOf(label)?.querySelectorAll("select option") ?? [])
    .map((o) => o.textContent?.trim())
    .filter(Boolean);
const shown = (label: string) => fieldOf(label) !== undefined;
const press = (el: HTMLElement) =>
  act(async () => {
    fireEvent.click(el);
  });

/** Every option the page lists reads as a label, never as a raw PWG or IPP keyword. */
function expectNoRawKeywords() {
  expect(document.body.textContent).not.toMatch(/[a-z]+_[a-z0-9.-]+_[0-9.]+x[0-9.]+(mm|in)/);
  for (const o of document.querySelectorAll("select option")) {
    const value = (o as HTMLOptionElement).value;
    if (/[-_]/.test(value)) expect(o.textContent?.trim()).not.toBe(value);
  }
}

describe("format detection mirrors the server", () => {
  test("signatures, text and unknown", () => {
    expect(detectFormat(bytes("%PDF-1.7"))).toBe("application/pdf");
    expect(detectFormat(bytes("%!PS-Adobe"))).toBe("application/postscript");
    expect(detectFormat(Uint8Array.of(0xff, 0xd8, 0xff, 0xe0))).toBe("image/jpeg");
    expect(detectFormat(bytes("\x89PNG\r\n\x1a\n...."))).toBe("image/png");
    expect(detectFormat(bytes("RaS2"))).toBe("image/pwg-raster");
    expect(detectFormat(bytes("UNIRAST\0"))).toBe("image/urf");
    expect(detectFormat(bytes("Hello\tworld\r\n\f"))).toBe("text/plain");
    // A multi-byte rune cut off by the 512-byte head is still text.
    expect(detectFormat(Uint8Array.of(0x61, 0xe2, 0x82))).toBe("text/plain");
    expect(detectFormat(Uint8Array.of(0x61, 0xff, 0x62))).toBe("application/octet-stream");
    expect(detectFormat(bytes("PK\x03\x04"))).toBe("application/octet-stream");
    expect(detectFormat(bytes("a\x7fb"))).toBe("application/octet-stream");
    expect(detectFormat(new Uint8Array())).toBe("application/octet-stream");
  });

  test("negotiation ignores parameters and never accepts unknown", () => {
    expect(formatAccepted("image/jpeg", ["Image/JPEG; charset=binary"])).toBe(true);
    expect(formatAccepted("image/png", ["image/jpeg"])).toBe(false);
    expect(formatAccepted("application/octet-stream", ["application/octet-stream"])).toBe(false);
  });
});

describe("Print view", () => {
  test("renders before the first Status arrives", async () => {
    await renderPrint(null);
    expect(screen.getByRole("heading", { level: 1, name: "Print" })).toBeTruthy();
    expect(screen.getByText("Checking the printer…")).toBeTruthy();
  });

  test("without a connected printer the controls are hidden or disabled and the reason is shown", async () => {
    await renderPrint({
      ...base,
      phase: "deploying",
      printers: [
        printer({
          phase: "probing",
          available: false,
          message: "Not plugged in, or not visible to pagewire",
        }),
      ],
    });
    expect(screen.getByText("The printer is not ready")).toBeTruthy();
    expect(screen.getByText("Not plugged in, or not visible to pagewire")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Choose files" }).hasAttribute("disabled")).toBe(
      true,
    );
    expect(printButton().hasAttribute("disabled")).toBe(true);
    // Its options are unknown until it is connected, so none is offered.
    expect(shown("Paper size")).toBe(false);
    expect(shown("Copies")).toBe(false);
    expect(screen.getByText("Jobs appear here once the printer is ready.")).toBeTruthy();
    // The jobs route answers 503 without a printer, so it is not asked.
    expect(calls.some((c) => c.path.endsWith("/jobs"))).toBe(false);
  });

  test("flags unsupported files before upload and sends only printable ones", async () => {
    const { upload, uploads } = fakeUpload();
    const { container } = await renderPrint(ready, upload);
    expect(screen.getByText(/This printer accepts PDF and JPEG\./)).toBeTruthy();
    await pick(container, [
      new File(["%PDF-1.7\n"], "boarding-pass.pdf", { type: "application/pdf" }),
      // Named .jpg, but the content decides.
      new File([bytes("\x89PNG\r\n\x1a\n")], "photo.jpg", { type: "image/jpeg" }),
      new File([bytes("PK\x03\x04\x14\0")], "notes.docx"),
    ]);
    await waitFor(() =>
      expect(within(row("boarding-pass.pdf")).getByText(/Ready to print/)).toBeTruthy(),
    );
    expect(within(row("photo.jpg")).getByText(/does not accept PNG files/)).toBeTruthy();
    expect(
      within(row("notes.docx")).getByText(/not a document format pagewire can send/),
    ).toBeTruthy();
    expect(row("notes.docx").textContent).toContain(
      "Open it in Word or Google Docs, choose Print > Save as PDF, then add the PDF here.",
    );
    expect(row("photo.jpg").textContent).toContain("export it as JPEG");

    expect(printButton().textContent).toBe("Print 1 of 3 files");
    await act(async () => {
      fireEvent.click(printButton());
    });
    expect(uploads.map((u) => [u.printer, u.file.name])).toEqual([["hp", "boarding-pass.pdf"]]);
  });

  test("options default from the printer's entry, and progress shows while a file uploads", async () => {
    configResponse = { etag: "e1", config, state: "applied", errors: [], lastGood: null };
    const { upload, uploads } = fakeUpload();
    const { container } = await renderPrint(ready, upload);
    await waitFor(() => expect(selected("Paper size")).toBe("Letter (8.5 × 11 in)"));
    expect(selected("Sides")).toBe("Two-sided, long edge");
    expect(selected("Colour")).toBe("Black and white");
    expect(selected("Quality")).toBe("High");
    // Not configured: the printer's own default.
    expect(selected("Paper tray")).toBe("Automatic");
    expect(screen.getByText("Up to 1 MB per file.", { exact: false })).toBeTruthy();

    const big = new Uint8Array(1024 * 1024 + 1);
    big.set(bytes("%PDF-"));
    await pick(container, [
      new File(["%PDF-1.7\n"], "a.pdf"),
      new File(["%PDF-1.7\n"], "b.pdf"),
      new File([big], "huge.pdf"),
    ]);
    await waitFor(() => expect(within(row("huge.pdf")).getByText(/upload limit/)).toBeTruthy());
    expect(printButton().textContent).toBe("Print 2 of 3 files");
    expect(screen.getByText("1 copy · Letter (8.5 × 11 in) · two-sided, long edge")).toBeTruthy();

    await act(async () => {
      fireEvent.click(printButton());
    });
    expect(uploads.length).toBe(1);
    // Every shown control is sent; the single-valued output tray and finishing are not, nor is
    // one copy.
    expect(uploads[0]?.options).toEqual({
      media: "na_letter_8.5x11in",
      mediaSource: "auto",
      mediaType: "stationery",
      sides: "two-sided-long-edge",
      colorMode: "monochrome",
      quality: "high",
      resolution: 600,
      contentOptimize: "auto",
      orientation: "portrait",
      numberUp: 1,
      printScaling: "auto",
    });
    act(() => uploads[0]?.progress(0.5));
    const bar = screen.getByRole("progressbar", { name: "Uploading a.pdf" });
    expect(bar.getAttribute("aria-valuenow")).toBe("50");
    // Pending, not disabled, so keyboard focus stays on the button.
    expect(printButton().hasAttribute("disabled")).toBe(false);
    expect(printButton().hasAttribute("data-pending")).toBe(true);
    // The batch is fixed once Print is pressed, so no row can be removed from it mid-way.
    expect(screen.getByRole("button", { name: "Remove b.pdf" }).hasAttribute("disabled")).toBe(
      true,
    );

    // One upload at a time: b.pdf starts only after a.pdf finished.
    await act(async () => uploads[0]?.done());
    expect(screen.queryByText("a.pdf")).toBeNull();
    expect(uploads.map((u) => u.file.name)).toEqual(["a.pdf", "b.pdf"]);
    await act(async () =>
      uploads[1]?.fail(
        new ProblemError({
          type: "about:blank",
          title: "Unsupported Media Type",
          status: 415,
          detail: "",
          errors: [],
        }),
      ),
    );
    expect(within(row("b.pdf")).getByText(/does not accept this file type/)).toBeTruthy();
    expect(screen.getByText("huge.pdf")).toBeTruthy();
  });

  test("the Samsung shows only paper size, quality, resolution and copies, and sends only those", async () => {
    const { upload, uploads } = fakeUpload();
    const { container } = await renderPrint({ ...base, printers: [samsung] }, upload);
    expect(shown("Paper size")).toBe(true);
    expect(shown("Quality")).toBe(true);
    expect(shown("Resolution")).toBe(true);
    expect(shown("Copies")).toBe(true);
    for (const label of [
      "Colour",
      "Sides",
      "Paper tray",
      "Paper type",
      "Orientation",
      "Pages per sheet",
      "Scaling",
      "Output tray",
      "Finishing",
    ]) {
      expect(shown(label)).toBe(false);
    }
    // No text stands in for the hidden controls either.
    expect(screen.queryByText("More options")).toBeNull();
    expect(screen.queryByText(/one-sided|black and white/i)).toBeNull();
    expect(screen.getByText("1 copy · Letter (8.5 × 11 in)")).toBeTruthy();

    await pick(container, [new File(["%PDF-1.7\n"], "a.pdf")]);
    await waitFor(() => expect(printButton().hasAttribute("disabled")).toBe(false));
    await press(printButton());
    expect(uploads[0]?.printer).toBe("samsung");
    expect(uploads[0]?.options).toEqual({
      media: "na_letter_8.5x11in",
      quality: "normal",
      resolution: 600,
    });
  });

  test("the HP shows trays and types, hides its single output tray and finishing, and keeps layout under More options", async () => {
    await renderPrint(ready);
    expect(optionsOf("Paper tray")).toEqual(["Automatic", "Tray 1", "Manual feed"]);
    expect(optionsOf("Paper type")).toEqual(["Plain paper", "Glossy photo paper"]);
    expect(optionsOf("Paper size")).toContain("Envelope #10 (4.125 × 9.5 in)");
    expect(shown("Output tray")).toBe(false);
    expect(shown("Finishing")).toBe(false);

    const more = screen.getByRole("button", { name: "More options" });
    await press(more);
    const disclosure = more.closest(".ui-disclosure") as HTMLElement;
    for (const label of ["Orientation", "Pages per sheet", "Scaling"]) {
      expect(within(disclosure).getAllByText(label).length).toBeGreaterThan(0);
    }
    expect(within(disclosure).queryByText("Paper tray")).toBeNull();
    expectNoRawKeywords();
  });

  test("the sides control is a dropdown", async () => {
    await renderPrint(ready);
    const sides = fieldOf("Sides")?.querySelector("button") as HTMLElement;
    expect(sides.getAttribute("aria-haspopup")).toBe("listbox");
  });

  test("a printer that allows one copy shows no copies field and sends no copies", async () => {
    const { upload, uploads } = fakeUpload();
    const { container } = await renderPrint(
      { ...base, printers: [printer({ capabilities: { ...hpCaps, copiesMax: 1 } })] },
      upload,
    );
    expect(shown("Copies")).toBe(false);
    await pick(container, [new File(["%PDF-1.7\n"], "a.pdf")]);
    await waitFor(() => expect(printButton().hasAttribute("disabled")).toBe(false));
    await press(printButton());
    expect(uploads[0]?.options).not.toHaveProperty("copies");
  });

  test("the response's warnings show in the toast with labels", async () => {
    const { upload, uploads } = fakeUpload();
    const { container } = await renderPrint(ready, upload);
    await pick(container, [new File(["%PDF-1.7\n"], "a.pdf")]);
    await waitFor(() => expect(printButton().hasAttribute("disabled")).toBe(false));
    await press(printButton());
    await act(async () =>
      uploads[0]?.done([{ option: "colorMode", value: "auto", using: "monochrome", message: "x" }]),
    );
    const sent = toastQueue.visibleToasts.find(
      (x) => x.content.title === "Sent a.pdf to the printer",
    );
    expect(sent?.content.description).toBe(
      "The default Colour “Automatic” is not offered by this printer; it uses “Black and white”.",
    );
  });

  test("a picker switches printers, recomputing the options and the jobs", async () => {
    jobs = [
      {
        id: 1,
        printer: "samsung",
        name: "samsung-job.pdf",
        state: "completed",
        reasons: [],
        createdAt: "2026-11-24T17:00:00Z",
      },
    ];
    await renderPrint(both);
    expect(shown("Sides")).toBe(true);
    expect(calls).toContainEqual({ method: "GET", path: "/api/v1/printers/hp/jobs" });
    await press(screen.getByRole("radio", { name: /^Samsung SCX-4300/ }));
    expect(shown("Sides")).toBe(false);
    await waitFor(() => expect(screen.getByText("samsung-job.pdf")).toBeTruthy());
    expect(calls).toContainEqual({ method: "GET", path: "/api/v1/printers/samsung/jobs" });
    // The choice is remembered for the next visit.
    expect(localStorage.getItem("pagewire:printer")).toBe("samsung");
  });

  test("files the server converts are accepted and listed", async () => {
    const { container } = await renderPrint({
      ...base,
      printers: [printer({ convertedFormats: [{ from: "image/png", to: "image/jpeg" }] })],
    });
    expect(screen.getByText(/This printer accepts PDF, JPEG, and PNG\./)).toBeTruthy();
    await pick(container, [new File([bytes("\x89PNG\r\n\x1a\n")], "scan.png")]);
    await waitFor(() => expect(within(row("scan.png")).getByText(/Ready to print/)).toBeTruthy());
    expect(printButton().textContent).toBe("Print");
  });

  test("conversion and load problems from the server show on the file's row", async () => {
    const { upload, uploads } = fakeUpload();
    const { container } = await renderPrint(ready, upload);
    await pick(container, [
      new File([Uint8Array.of(0xff, 0xd8, 0xff, 0xee)], "cmyk.jpg"),
      new File([Uint8Array.of(0xff, 0xd8, 0xff, 0xe0)], "huge.jpg"),
      new File([Uint8Array.of(0xff, 0xd8, 0xff, 0xe0)], "broken.jpg"),
      new File([Uint8Array.of(0xff, 0xd8, 0xff, 0xe0)], "busy.jpg"),
    ]);
    await waitFor(() => expect(printButton().textContent).toBe("Print 4 files"));
    await act(async () => {
      fireEvent.click(printButton());
    });
    const cmyk =
      "This JPEG uses a colour format the printer path cannot embed. Save it as an RGB JPEG or a PDF.";
    await act(async () => uploads[0]?.fail(problem(415, "unsupported-format", cmyk)));
    await act(async () =>
      uploads[1]?.fail(
        problem(413, "image-too-large", "the image is 30000 x 30000 pixels, too large to convert"),
      ),
    );
    await act(async () => uploads[2]?.fail(problem(400, "invalid-image", "the file is damaged")));
    await act(async () =>
      uploads[3]?.fail(
        problem(429, "too-many-jobs", "another image is being converted; try again in a moment"),
      ),
    );
    expect(within(row("cmyk.jpg")).getByText(cmyk)).toBeTruthy();
    expect(row("huge.jpg").textContent).toContain("30000 x 30000 pixels");
    expect(row("broken.jpg").textContent).toContain("damaged");
    expect(row("busy.jpg").textContent).toContain("try again in a moment");
  });

  test("copies stay in the button and the summary, which follows both cards", async () => {
    const { upload, uploads } = fakeUpload();
    const { container } = await renderPrint(ready, upload);
    await pick(container, [new File(["%PDF-1.7\n"], "a.pdf"), new File(["%PDF-1.7\n"], "b.pdf")]);
    const copies = screen.getByRole("textbox", { name: "Copies" }) as HTMLInputElement;
    await act(async () => {
      fireEvent.change(copies, { target: { value: "3" } });
      fireEvent.blur(copies);
    });
    await waitFor(() => expect(printButton().textContent).toBe("Print 2 files, 3 copies each"));
    expect(screen.getByText("3 copies · A4 (210 × 297 mm) · one-sided")).toBeTruthy();
    const options = screen.getByRole("heading", { name: "Options" });
    // The submit row comes after the Options card in reading order.
    expect(
      options.compareDocumentPosition(printButton()) & Node.DOCUMENT_POSITION_FOLLOWING,
    ).toBeTruthy();
    await press(printButton());
    expect(uploads[0]?.options.copies).toBe(3);
  });

  test("printing is off while the connection to pagewire is down", async () => {
    const { container } = await renderPrint(ready, fakeUpload().upload, false);
    await pick(container, [new File(["%PDF-1.7\n"], "a.pdf")]);
    await waitFor(() => expect(within(row("a.pdf")).getByText(/Ready to print/)).toBeTruthy());
    expect(printButton().hasAttribute("disabled")).toBe(true);
  });

  test("while pagewire finds no device it tells you to plug it in", async () => {
    await renderPrint({ ...base, phase: "probing" });
    expect(await screen.findByText(/Plug the printer in with USB/)).toBeTruthy();
  });

  test("a cleared copies field blocks printing", async () => {
    const { container } = await renderPrint(ready, fakeUpload().upload);
    await pick(container, [new File(["%PDF-1.7\n"], "a.pdf")]);
    await waitFor(() => expect(printButton().hasAttribute("disabled")).toBe(false));
    const copies = screen.getByRole("textbox", { name: "Copies" }) as HTMLInputElement;
    await act(async () => {
      fireEvent.change(copies, { target: { value: "" } });
      fireEvent.blur(copies);
    });
    expect(screen.getByText("Enter 1 to 99 copies.")).toBeTruthy();
    expect(printButton().hasAttribute("disabled")).toBe(true);
  });

  test("a printing job uses a different badge tone than a waiting one", async () => {
    const job: Job = {
      id: 1,
      printer: "hp",
      name: "waiting.pdf",
      state: "pending",
      reasons: [],
      createdAt: "2026-11-24T17:00:00+01:00",
    };
    jobs = [job, { ...job, id: 2, name: "printing.pdf", state: "processing" }];
    await renderPrint(ready);
    await waitFor(() => expect(within(row("printing.pdf")).getByText("Printing")).toBeTruthy());
    expect(calls).toContainEqual({ method: "GET", path: "/api/v1/printers/hp/jobs" });
    const tone = (name: string) => row(name).querySelector(".ui-badge")?.getAttribute("data-tone");
    expect(tone("waiting.pdf")).toBe("busy");
    expect(tone("printing.pdf")).toBe("active");
  });

  test("a job row moves from pending to completed after a job event, and can be canceled while pending", async () => {
    const job: Job = {
      id: 7,
      printer: "hp",
      name: "boarding-pass.pdf",
      state: "pending",
      reasons: [],
      createdAt: "2026-11-24T17:00:00+01:00",
    };
    // A job from a LAN client with no creation time carries the zero time.
    const lanJob: Job = {
      ...job,
      id: 3,
      name: "lan.ps",
      state: "completed",
      createdAt: "0001-01-01T00:00:00Z",
    };
    jobs = [job, lanJob];
    await renderPrint(ready);
    await waitFor(() => expect(within(row("lan.ps")).getByText("Sent to printer")).toBeTruthy());
    expect(row("lan.ps").querySelector(".print-subtle")?.textContent).toBe("");
    await waitFor(() => expect(within(row("boarding-pass.pdf")).getByText("Waiting")).toBeTruthy());

    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Cancel boarding-pass.pdf" }));
    });
    expect(calls).toContainEqual({ method: "DELETE", path: "/api/v1/printers/hp/jobs/7" });

    emit("job", { ...job, state: "completed" });
    await waitFor(() =>
      expect(within(row("boarding-pass.pdf")).getByText("Sent to printer")).toBeTruthy(),
    );
    expect(screen.getAllByText("boarding-pass.pdf").length).toBe(1);
    expect(screen.queryByRole("button", { name: "Cancel boarding-pass.pdf" })).toBeNull();
  });

  test("job rows from the first load do not arrive; a new job from an event or from Print does", async () => {
    const old: Job = {
      id: 7,
      printer: "hp",
      name: "old.pdf",
      state: "completed",
      reasons: [],
      createdAt: "2026-11-24T17:00:00Z",
    };
    jobs = [old];
    const { upload, uploads } = fakeUpload();
    const { container } = await renderPrint(ready, upload);
    await waitFor(() => expect(screen.getByText("old.pdf")).toBeTruthy());
    expect(row("old.pdf").hasAttribute("data-arrived")).toBe(false);

    emit("job", { ...old, id: 8, name: "lan.ps", state: "pending" });
    expect(row("lan.ps").hasAttribute("data-arrived")).toBe(true);
    // A state change of a job already listed is not an arrival.
    emit("job", { ...old, state: "aborted" });
    expect(row("old.pdf").hasAttribute("data-arrived")).toBe(false);

    await pick(container, [new File(["%PDF-1.7\n"], "mine.pdf")]);
    await waitFor(() => expect(printButton().hasAttribute("disabled")).toBe(false));
    await press(printButton());
    await act(async () => uploads[0]?.done());
    // The upload returns job 1; it reaches the list through the job event that follows.
    emit("job", { ...old, id: 1, name: "mine.pdf", state: "pending" });
    const jobRow = within(container.querySelector(".print-jobs") as HTMLElement)
      .getByText("mine.pdf")
      .closest("li") as HTMLElement;
    expect(jobRow.hasAttribute("data-arrived")).toBe(true);
  });

  test("switching printers takes a fresh first load as the baseline for each", async () => {
    const job = (id: number, p: string, name: string): Job => ({
      id,
      printer: p,
      name,
      state: "completed",
      reasons: [],
      createdAt: "2026-11-24T17:00:00Z",
    });
    jobs = [job(1, "hp", "a1.pdf"), job(2, "samsung", "b1.pdf")];
    await renderPrint(both);
    await waitFor(() => expect(screen.getByText("a1.pdf")).toBeTruthy());
    expect(row("a1.pdf").hasAttribute("data-arrived")).toBe(false);

    await press(screen.getByRole("radio", { name: /^Samsung SCX-4300/ }));
    await waitFor(() => expect(screen.getByText("b1.pdf")).toBeTruthy());
    expect(row("b1.pdf").hasAttribute("data-arrived")).toBe(false);

    // A new job on the HP while the Samsung is shown: the HP's next first load includes it.
    const a2 = job(3, "hp", "a2.pdf");
    jobs.push(a2);
    emit("job", a2);
    expect(screen.queryByText("a2.pdf")).toBeNull();

    await press(screen.getByRole("radio", { name: /^HP OfficeJet/ }));
    await waitFor(() => expect(screen.getByText("a2.pdf")).toBeTruthy());
    expect(document.querySelectorAll("[data-arrived]").length).toBe(0);

    emit("job", job(4, "hp", "a3.pdf"));
    expect(row("a3.pdf").hasAttribute("data-arrived")).toBe(true);
    expect(row("a1.pdf").hasAttribute("data-arrived")).toBe(false);
  });
});
