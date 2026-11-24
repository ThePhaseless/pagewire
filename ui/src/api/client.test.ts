import { afterEach, beforeEach, describe, expect, mock, test } from "bun:test";
import { act, cleanup, render, renderHook, screen, waitFor } from "@testing-library/react";
import { createElement as h, type ReactNode } from "react";
import type { Config, ConfigResponse, Job, Status } from "../gen/api";
import {
  API,
  api,
  ConflictError,
  ProblemError,
  printFormData,
  problemMessage,
  uploadPrint,
} from "./client";
import {
  createEventStore,
  type EventStore,
  EventsContext,
  mergeJobs,
  useEvent,
  useJobs,
  useStatus,
} from "./events";

// The Status the controller returns before any input (plan: controller rules, "Initial Status").
const initialStatus = {
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

const simulatedDecision = {
  printer: "sim",
  scanner: "sim",
  saneBackends: [],
  requiredEulas: [],
  optionalEulas: [],
  rule: "simulated",
  device: null,
};

const config: Config = {
  apiVersion: "printscan/v1",
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
    defaults: { source: "platen", resolution: 300, colorMode: "color", format: "pdf" },
    retentionDays: 7,
  },
  licenses: { acceptedEulas: [] },
};

function readyStatus(over: { printerState?: string; available?: boolean } = {}): Status {
  return {
    ...initialStatus,
    config: {
      state: "applied",
      etag: "e1",
      appliedEtag: "e1",
      errors: [],
      changedAt: "2026-11-24T18:00:00.5+01:00",
    },
    device: {
      ...initialStatus.device,
      phase: "ready",
      mode: "simulated",
      decision: simulatedDecision,
      inventory: { devices: [], probedAt: "2026-11-24T18:00:00+01:00", simulated: true },
    },
    printer: {
      ...initialStatus.printer,
      available: over.available ?? true,
      state: over.printerState ?? "idle",
      uri: "ipp://127.0.0.1:8631/ipp/print",
    },
    reconcile: [
      { item: "device", ok: true, message: "", at: "2026-11-24T18:00:01.123456789+01:00" },
    ],
  } as Status;
}

const job = (id: number, state: string, createdAt = "2026-11-24T18:00:00+01:00"): Job =>
  ({ id, name: `doc-${id}.pdf`, state, reasons: [], createdAt }) as Job;

const realFetch = globalThis.fetch;
let calls: { url: string; init: RequestInit }[] = [];

function respond(status: number, body: unknown, contentType = "application/json") {
  const fn = mock(async (url: string | URL | Request, init: RequestInit = {}) => {
    calls.push({ url: String(url), init });
    return new Response(typeof body === "string" ? body : JSON.stringify(body), {
      status,
      headers: { "Content-Type": contentType },
    });
  });
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
}

beforeEach(() => {
  calls = [];
});
afterEach(() => {
  globalThis.fetch = realFetch;
  cleanup();
});

describe("response parsing", () => {
  test("the initial Status (device.mode none, printer.state unknown, zero changedAt)", async () => {
    respond(200, initialStatus);
    const s = await api.getStatus();
    expect(s.device.mode).toBe("none");
    expect(s.device.phase).toBe("none");
    expect(s.printer.state).toBe("unknown");
    expect(s.config.state).toBe("missing");
    expect(s.config.changedAt).toBe("0001-01-01T00:00:00Z");
    expect(calls[0]?.url).toBe(`${API}/status`);
  });

  test("a simulated decision with device: null and +01:00 timestamps", async () => {
    respond(200, readyStatus());
    const s = await api.getStatus();
    expect(s.device.decision?.device).toBeNull();
    expect(s.device.decision?.rule).toBe("simulated");
    expect(s.reconcile[0]?.at).toBe("2026-11-24T18:00:01.123456789+01:00");
  });

  test("a config without device.usb, and config: null with lastGood: null", async () => {
    respond(200, { etag: "abc", config, state: "applied", errors: [], lastGood: null });
    const ok = await api.getConfig();
    expect(ok.config?.device.usb).toBeUndefined();

    respond(200, {
      etag: "def",
      config: null,
      state: "invalid",
      errors: [{ path: "/device/mode", line: 4, column: 9, message: "bad enum" }],
      lastGood: null,
    });
    const bad = await api.getConfig();
    expect(bad.config).toBeNull();
    expect(bad.lastGood).toBeNull();
    expect(bad.errors[0]?.line).toBe(4);
  });

  test("empty arrays", async () => {
    respond(200, []);
    expect(await api.getJobs()).toEqual([]);
  });

  test("a response that does not match the schema becomes a bad-response problem", async () => {
    const err = mock(() => {});
    const realError = console.error;
    console.error = err;
    try {
      respond(200, { ...initialStatus, device: { ...initialStatus.device, mode: "" } });
      const e = await api.getStatus().catch((x) => x);
      expect(e).toBeInstanceOf(ProblemError);
      expect((e as ProblemError).type).toBe("bad-response");
    } finally {
      console.error = realError;
    }
  });
});

describe("problems", () => {
  test("a problem+json body becomes a ProblemError with field errors", async () => {
    respond(
      422,
      {
        type: "validation",
        title: "Invalid config",
        status: 422,
        detail: "1 error",
        errors: [{ path: "/name", line: 2, column: 7, message: "too long" }],
      },
      "application/problem+json",
    );
    const e = (await api.patchConfig({ name: "x" }, "abc").catch((x) => x)) as ProblemError;
    expect(e).toBeInstanceOf(ProblemError);
    expect(e.status).toBe(422);
    expect(e.errors[0]?.path).toBe("/name");
  });

  test("problem types map to shared copy", () => {
    const p = (type: string, status: number) =>
      new ProblemError({ type, title: "t", status, detail: "", errors: [] });
    expect(problemMessage(p("storage-error", 500))).toContain("data directory");
    expect(problemMessage(p("multipart-order", 400))).toContain("upload was malformed");
    expect(problemMessage(p("whatever", 503))).toContain("not available");
    expect(problemMessage(new Error("x"))).toBe("Something went wrong.");
  });

  test("a non-JSON error body still becomes a ProblemError", async () => {
    respond(500, "boom", "text/plain");
    const e = (await api.getSetup().catch((x) => x)) as ProblemError;
    expect(e.status).toBe(500);
    expect(e.type).toBe("about:blank");
  });

  test("a network failure becomes a network-error problem", async () => {
    globalThis.fetch = (async () => {
      throw new TypeError("Failed to fetch");
    }) as unknown as typeof fetch;
    const e = (await api.getStatus().catch((x) => x)) as ProblemError;
    expect(e.type).toBe("network-error");
  });
});

describe("config writes", () => {
  test("PATCH sends a merge patch with a quoted If-Match", async () => {
    respond(200, { etag: "new", config, state: "applied", errors: [], lastGood: config });
    const r = await api.patchConfig({ location: "Hall" }, "abc");
    expect(r.etag).toBe("new");
    const headers = calls[0]?.init.headers as Record<string, string>;
    expect(calls[0]?.init.method).toBe("PATCH");
    expect(headers["If-Match"]).toBe('"abc"');
    expect(headers["Content-Type"]).toBe("application/merge-patch+json");
    expect(JSON.parse(String(calls[0]?.init.body))).toEqual({ location: "Hall" });
  });

  test("create sends If-None-Match: *", async () => {
    respond(201, { etag: "new", config, state: "applied", errors: [], lastGood: config });
    await api.createConfig(config);
    const headers = calls[0]?.init.headers as Record<string, string>;
    expect(headers["If-None-Match"]).toBe("*");
    expect(calls[0]?.init.method).toBe("PUT");
  });

  test("a 412 against a valid file surfaces a ConflictError with the current etag and config", async () => {
    const onDisk: ConfigResponse = {
      etag: "disk",
      config: { ...config, name: "Changed on disk" },
      state: "applied",
      errors: [],
      lastGood: null,
    };
    respond(412, onDisk);
    const e = (await api.patchConfig({ name: "Mine" }, "stale").catch((x) => x)) as ConflictError;
    expect(e).toBeInstanceOf(ConflictError);
    expect(e.etag).toBe("disk");
    expect(e.current.config?.name).toBe("Changed on disk");
  });

  test("a 412 against an invalid file surfaces config: null and state: invalid", async () => {
    respond(412, {
      etag: "disk",
      config: null,
      state: "invalid",
      errors: [{ path: "", line: 3, column: 1, message: "syntax error" }],
      lastGood: config,
    });
    const e = (await api.replaceConfig(config, "stale").catch((x) => x)) as ConflictError;
    expect(e).toBeInstanceOf(ConflictError);
    expect(e.current.config).toBeNull();
    expect(e.current.state).toBe("invalid");
    expect(e.current.lastGood?.name).toBe(config.name);
  });
});

describe("print upload", () => {
  const options = {
    copies: 2,
    sides: "one-sided",
    colorMode: "auto",
    media: "iso_a4_210x297mm",
    quality: "normal",
  } as const;

  test("FormData puts options before file", async () => {
    const fd = printFormData(new File(["%PDF-1.4"], "a.pdf", { type: "application/pdf" }), options);
    expect([...fd.keys()]).toEqual(["options", "file"]);
    expect(JSON.parse(String(fd.get("options")))).toEqual(options);
    expect((fd.get("file") as File).name).toBe("a.pdf");
  });

  test("uploadPrint posts the ordered FormData, reports progress and parses the Job", async () => {
    let sent: FormData | undefined;
    const progress: number[] = [];
    const fake = {
      upload: { onprogress: null as ((e: ProgressEvent) => void) | null },
      onload: null as (() => void) | null,
      onerror: null as (() => void) | null,
      status: 0,
      statusText: "",
      responseText: "",
      method: "",
      url: "",
      open(method: string, url: string) {
        this.method = method;
        this.url = url;
      },
      setRequestHeader() {},
      send(body: FormData) {
        sent = body;
        this.upload.onprogress?.({
          lengthComputable: true,
          loaded: 50,
          total: 100,
        } as ProgressEvent);
        this.status = 202;
        this.responseText = JSON.stringify(job(7, "pending"));
        this.onload?.();
      },
    };
    const j = await uploadPrint(
      new File(["x"], "a.pdf"),
      options,
      (f) => progress.push(f),
      () => fake as unknown as XMLHttpRequest,
    );
    expect(fake.method).toBe("POST");
    expect(fake.url).toBe(`${API}/print`);
    expect([...(sent?.keys() ?? [])]).toEqual(["options", "file"]);
    expect(progress).toEqual([0.5]);
    expect(j.id).toBe(7);
  });
});

class FakeEventSource {
  static instances: FakeEventSource[] = [];
  onopen: ((ev: Event) => unknown) | null = null;
  onerror: ((ev: Event) => unknown) | null = null;
  closed = false;
  listeners = new Map<string, ((e: MessageEvent) => void)[]>();
  constructor(public url: string) {
    FakeEventSource.instances.push(this);
  }
  addEventListener(type: string, fn: (e: MessageEvent) => void) {
    this.listeners.set(type, [...(this.listeners.get(type) ?? []), fn]);
  }
  close() {
    this.closed = true;
  }
  open() {
    this.onopen?.(new Event("open"));
  }
  fail() {
    this.onerror?.(new Event("error"));
  }
  emit(type: string, data: unknown) {
    for (const fn of this.listeners.get(type) ?? []) {
      fn(new MessageEvent(type, { data: typeof data === "string" ? data : JSON.stringify(data) }));
    }
  }
}

const lastSource = () => FakeEventSource.instances.at(-1) as FakeEventSource;

function fakeStore(): EventStore {
  FakeEventSource.instances = [];
  return createEventStore({
    newEventSource: (u) => new FakeEventSource(u) as unknown as EventSource,
    minBackoffMs: 1,
    maxBackoffMs: 4,
  });
}

function Probe() {
  const status = useStatus();
  const scan = useEvent("scan");
  return h("p", null, `${status?.device.phase ?? "no status"} / ${scan?.pages ?? "no scan"}`);
}

describe("SSE store", () => {
  test("reduces status, config, job and scan events and reconnects with backoff", async () => {
    const store = fakeStore();
    const jobs: number[] = [];
    let reconnects = 0;
    store.on("job", (j) => jobs.push(j.id));
    store.onReconnect(() => reconnects++);
    store.start();
    expect(lastSource().url).toBe(`${API}/events`);

    lastSource().open();
    expect(store.getSnapshot().connected).toBe(true);
    lastSource().emit("status", initialStatus);
    expect(store.getSnapshot().status?.device.mode).toBe("none");
    expect(store.getSnapshot().lastEventAt).not.toBeNull();
    lastSource().emit("config", { etag: "e2" });
    expect(store.latest("config")?.etag).toBe("e2");
    lastSource().emit("job", job(3, "processing"));
    expect(jobs).toEqual([3]);

    // A malformed event is dropped and logged; the store keeps going.
    const realError = console.error;
    console.error = mock(() => {});
    lastSource().emit("job", { id: "nope" });
    lastSource().emit("status", "{not json");
    console.error = realError;
    expect(jobs).toEqual([3]);
    expect(store.getSnapshot().status?.device.mode).toBe("none");

    const first = lastSource();
    first.fail();
    expect(first.closed).toBe(true);
    expect(store.getSnapshot().connected).toBe(false);
    await waitFor(() => expect(FakeEventSource.instances.length).toBe(2));
    expect(reconnects).toBe(0);
    lastSource().open();
    expect(reconnects).toBe(1);
    expect(store.getSnapshot().connected).toBe(true);

    store.stop();
    expect(lastSource().closed).toBe(true);
  });

  test("useStatus and useEvent re-render on events", async () => {
    const store = fakeStore();
    store.start();
    render(h(EventsContext, { value: store }, h(Probe)));
    expect(screen.getByText("no status / no scan")).toBeTruthy();
    act(() => {
      lastSource().emit("status", readyStatus());
      lastSource().emit("scan", {
        id: "0123456789abcdef",
        state: "scanning",
        createdAt: "2026-11-24T18:00:00+01:00",
        pages: 2,
        format: "pdf",
        settings: { source: "adf", resolution: 300, colorMode: "color", format: "pdf" },
        error: null,
      });
    });
    expect(screen.getByText("ready / 2")).toBeTruthy();
    store.stop();
  });
});

describe("useJobs", () => {
  test("mergeJobs keeps one entry per id, newest first, at most 20", () => {
    const many = Array.from({ length: 25 }, (_, i) =>
      job(i + 1, "completed", new Date(Date.UTC(2026, 10, 24, 10, i)).toISOString()),
    );
    const merged = mergeJobs(many, [job(25, "canceled", many[24]?.createdAt)]);
    expect(merged).toHaveLength(20);
    expect(merged[0]?.id).toBe(25);
    expect(merged[0]?.state).toBe("canceled");
  });

  test("merges pending then completed for one id and refetches when the printer state changes", async () => {
    const store = fakeStore();
    store.start();
    const source = lastSource();
    source.open();
    source.emit("status", readyStatus({ printerState: "idle" }));

    let fetches = 0;
    const getJobs = mock(async () => {
      fetches++;
      return fetches === 1
        ? [job(1, "completed", "2026-11-24T17:00:00+01:00")]
        : [job(9, "completed")];
    });
    const wrapper = ({ children }: { children: ReactNode }) =>
      h(EventsContext, { value: store }, children);
    const { result } = renderHook(() => useJobs(getJobs), { wrapper });
    await waitFor(() => expect(result.current.jobs).toHaveLength(1));
    expect(fetches).toBe(1);

    act(() => source.emit("job", job(2, "pending", "2026-11-24T18:00:00+01:00")));
    act(() => source.emit("job", job(2, "completed", "2026-11-24T18:00:00+01:00")));
    expect(result.current.jobs.map((j) => [j.id, j.state])).toEqual([
      [2, "completed"],
      [1, "completed"],
    ]);

    act(() => source.emit("status", readyStatus({ printerState: "processing" })));
    await waitFor(() => expect(fetches).toBe(2));
    await waitFor(() => expect(result.current.jobs.map((j) => j.id)).toEqual([9]));
    store.stop();
  });

  test("does not ask for jobs while no printer is available, and refetches on reconnect", async () => {
    const store = fakeStore();
    store.start();
    lastSource().open();
    lastSource().emit("status", readyStatus({ available: false }));
    const getJobs = mock(async () => [] as Job[]);
    const wrapper = ({ children }: { children: ReactNode }) =>
      h(EventsContext, { value: store }, children);
    renderHook(() => useJobs(getJobs), { wrapper });
    await new Promise((r) => setTimeout(r, 10));
    expect(getJobs).toHaveBeenCalledTimes(0);

    act(() => lastSource().emit("status", readyStatus()));
    await waitFor(() => expect(getJobs).toHaveBeenCalledTimes(1));
    act(() => lastSource().fail());
    await waitFor(() => expect(FakeEventSource.instances.length).toBe(2));
    act(() => lastSource().open());
    await waitFor(() => expect(getJobs).toHaveBeenCalledTimes(2));
    store.stop();
  });
});
