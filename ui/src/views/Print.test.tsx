import { afterEach, beforeEach, describe, expect, spyOn, test } from "bun:test";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { ProblemError, type uploadPrint } from "../api/client";
import { createEventStore, EventsContext } from "../api/events";
import type { Config, ConfigResponse, Job, PrintOptions, Status } from "../gen/api";
import { detectFormat, formatAccepted, mediaLabel, Print } from "./Print";

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
  if (path === "/api/v1/jobs" && method === "GET") return json(jobs);
  if (path.startsWith("/api/v1/jobs/") && method === "DELETE")
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

const noPrinter: Status = {
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
    media: [],
    markers: [],
  },
  scanner: { available: false, url: "", state: "unknown" },
  reconcile: [],
  connect: { dnssdName: "", ippUris: [], esclUrls: [] },
};

const ready: Status = {
  ...noPrinter,
  config: { ...noPrinter.config, state: "applied", etag: "e1", appliedEtag: "e1" },
  device: { ...noPrinter.device, phase: "ready", mode: "simulated" },
  printer: {
    ...noPrinter.printer,
    available: true,
    uri: "ipp://127.0.0.1:8631/ipp/print",
    state: "idle",
    // octet-stream never makes an unknown file printable (internal/printing.Negotiate).
    formats: ["application/pdf", "image/jpeg; charset=binary", "application/octet-stream"],
    media: ["iso_a4_210x297mm", "na_letter_8.5x11in"],
  },
};

const config: Config = {
  apiVersion: "printscan/v1",
  name: "Living room printer",
  location: "",
  device: { mode: "simulated", driver: "auto", scanner: "auto" },
  print: {
    defaults: {
      media: "na_letter_8.5x11in",
      sides: "two-sided-long-edge",
      colorMode: "monochrome",
      quality: "high",
    },
    maxUploadMiB: 1,
  },
  scan: {
    defaults: { source: "platen", resolution: 300, colorMode: "color", format: "pdf" },
    retentionDays: 7,
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

const emit = (type: string, data: unknown) => act(() => FakeEventSource.last?.emit(type, data));

type Upload = {
  file: File;
  options: PrintOptions;
  progress: (f: number) => void;
  done: () => void;
  fail: (e: unknown) => void;
};

/** A controllable upload: each call waits until the test calls done() or fail(). */
function fakeUpload() {
  const uploads: Upload[] = [];
  const upload = ((file: File, options: PrintOptions, onProgress?: (f: number) => void) =>
    new Promise<Job>((resolve, reject) => {
      uploads.push({
        file,
        options,
        progress: (f) => onProgress?.(f),
        done: () =>
          resolve({
            id: 1,
            name: file.name,
            state: "pending",
            reasons: [],
            createdAt: "2026-11-24T17:00:00Z",
          }),
        fail: reject,
      });
    })) as unknown as typeof uploadPrint;
  return { upload, uploads };
}

async function renderPrint(status: Status | null, upload?: typeof uploadPrint) {
  const store = createEventStore({
    newEventSource: () => new FakeEventSource() as unknown as EventSource,
  });
  store.start();
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
const printButton = () => screen.getByRole("button", { name: /^Print/ });
/** The visible value of the Select labelled `label` (the hidden native select lists every option). */
const selected = (label: string) =>
  screen
    .getAllByText(label)
    .find((e) => e.classList.contains("ui-label"))
    ?.closest(".ui-field")
    ?.querySelector(".ui-select-value")?.textContent;

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

  test("media names", () => {
    expect(mediaLabel("iso_a4_210x297mm")).toBe("A4 (210 × 297 mm)");
    expect(mediaLabel("na_letter_8.5x11in")).toBe("Letter (8.5 × 11 in)");
    expect(mediaLabel("custom")).toBe("custom");
  });
});

describe("Print view", () => {
  test("renders before the first Status arrives", async () => {
    await renderPrint(null);
    expect(screen.getByRole("heading", { level: 1, name: "Print" })).toBeTruthy();
    expect(screen.getByText("Checking the printer…")).toBeTruthy();
  });

  test("without a printer the controls are disabled and the reason is shown", async () => {
    await renderPrint({
      ...noPrinter,
      device: { ...noPrinter.device, phase: "probing", message: "No printer found on USB" },
    });
    expect(screen.getByText("The printer is not ready")).toBeTruthy();
    expect(screen.getByText("No printer found on USB")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Choose files" }).hasAttribute("disabled")).toBe(
      true,
    );
    expect(printButton().hasAttribute("disabled")).toBe(true);
    expect(screen.getByText("Jobs appear here once the printer is ready.")).toBeTruthy();
    // GET /jobs answers 503 without a printer, so it is not asked.
    expect(calls.some((c) => c.path === "/api/v1/jobs")).toBe(false);
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
      within(row("notes.docx")).getByText(/not a document format printscan can send/),
    ).toBeTruthy();

    expect(printButton().textContent).toBe("Print");
    await act(async () => {
      fireEvent.click(printButton());
    });
    expect(uploads.map((u) => u.file.name)).toEqual(["boarding-pass.pdf"]);
  });

  test("options default from the config, and progress shows while a file uploads", async () => {
    configResponse = { etag: "e1", config, state: "applied", errors: [], lastGood: null };
    const { upload, uploads } = fakeUpload();
    const { container } = await renderPrint(ready, upload);
    await waitFor(() => expect(selected("Paper size")).toBe("Letter (8.5 × 11 in)"));
    expect(selected("Sides")).toBe("Two-sided, flip on long edge");
    expect(selected("Colour")).toBe("Black and white");
    expect(selected("Quality")).toBe("High");
    expect(screen.getByText("Up to 1 MiB per file.", { exact: false })).toBeTruthy();

    const big = new Uint8Array(1024 * 1024 + 1);
    big.set(bytes("%PDF-"));
    await pick(container, [
      new File(["%PDF-1.7\n"], "a.pdf"),
      new File(["%PDF-1.7\n"], "b.pdf"),
      new File([big], "huge.pdf"),
    ]);
    await waitFor(() => expect(within(row("huge.pdf")).getByText(/upload limit/)).toBeTruthy());
    expect(printButton().textContent).toBe("Print 2 files");

    await act(async () => {
      fireEvent.click(printButton());
    });
    expect(uploads.length).toBe(1);
    expect(uploads[0]?.options).toEqual({
      copies: 1,
      sides: "two-sided-long-edge",
      colorMode: "monochrome",
      media: "na_letter_8.5x11in",
      quality: "high",
    });
    act(() => uploads[0]?.progress(0.5));
    const bar = screen.getByRole("progressbar", { name: "Uploading a.pdf" });
    expect(bar.getAttribute("aria-valuenow")).toBe("50");
    expect(printButton().hasAttribute("disabled")).toBe(true);
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

  test("a job row moves from pending to completed after a job event, and can be canceled while pending", async () => {
    const job: Job = {
      id: 7,
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
    await waitFor(() => expect(within(row("lan.ps")).getByText("Printed")).toBeTruthy());
    expect(row("lan.ps").querySelector(".print-subtle")?.textContent).toBe("");
    await waitFor(() => expect(within(row("boarding-pass.pdf")).getByText("Waiting")).toBeTruthy());

    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Cancel boarding-pass.pdf" }));
    });
    expect(calls).toContainEqual({ method: "DELETE", path: "/api/v1/jobs/7" });

    emit("job", { ...job, state: "completed" });
    await waitFor(() => expect(within(row("boarding-pass.pdf")).getByText("Printed")).toBeTruthy());
    expect(screen.getAllByText("boarding-pass.pdf").length).toBe(1);
    expect(screen.queryByRole("button", { name: "Cancel boarding-pass.pdf" })).toBeNull();
  });
});
