import { afterEach, beforeEach, describe, expect, spyOn, test } from "bun:test";
import { act, cleanup, render, screen, waitFor, within } from "@testing-library/react";
import { createEventStore, type EventStore, EventsContext } from "../api/events";
import type { Job, Status as StatusDTO } from "../gen/api";
import { Status } from "./Status";

const realFetch = globalThis.fetch;
const jobsResponse = (jobs: Job[]) =>
  (async () =>
    new Response(JSON.stringify(jobs), {
      headers: { "Content-Type": "application/json" },
    })) as unknown as typeof fetch;
// useJobs() asks GET /jobs while the printer is available; never reach the network.
let consoleError: ReturnType<typeof spyOn>;
beforeEach(() => {
  globalThis.fetch = jobsResponse([]);
  consoleError = spyOn(console, "error");
});
afterEach(() => {
  // React reports duplicate keys and invalid markup through console.error.
  expect(consoleError).not.toHaveBeenCalled();
  consoleError.mockRestore();
  cleanup();
  globalThis.fetch = realFetch;
});

// The Status the controller returns before any input (plan: controller rules, "Initial Status").
const initialStatus: StatusDTO = {
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

const simulatedReady: StatusDTO = {
  ...initialStatus,
  config: { ...initialStatus.config, state: "applied", etag: "e1", appliedEtag: "e1" },
  device: {
    ...initialStatus.device,
    phase: "ready",
    mode: "simulated",
    decision: {
      printer: "sim",
      scanner: "sim",
      saneBackends: [],
      requiredEulas: [],
      optionalEulas: [],
      rule: "simulated",
      device: null,
    },
    advertised: [{ type: "_ipp._tcp", name: "Living room printer", port: 8631 }],
  },
  printer: {
    ...initialStatus.printer,
    available: true,
    uri: "ipp://127.0.0.1:8631/ipp/print",
    state: "idle",
    reasons: ["media-empty-error", "none", "some-new-thing-report"],
    markers: [
      { name: "Black toner", color: "#000000", level: 42 },
      { name: "Waste", color: "none", level: -2 },
    ],
  },
  scanner: { available: true, url: "http://127.0.0.1:8090/eSCL", state: "idle" },
  connect: {
    dnssdName: "Living room printer",
    ippUris: ["ipp://192.168.1.20:8631/ipp/print"],
    esclUrls: ["http://192.168.1.20:8090/eSCL"],
  },
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

async function renderWith(status: StatusDTO | null): Promise<EventStore> {
  const store = createEventStore({
    newEventSource: () => new FakeEventSource() as unknown as EventSource,
  });
  store.start();
  if (status) act(() => FakeEventSource.last?.emit("status", status));
  // Async act lets useJobs() settle its GET /jobs inside act.
  await act(async () => {
    render(
      <EventsContext value={store}>
        <Status />
      </EventsContext>,
    );
  });
  return store;
}

const emit = (type: string, data: unknown) => act(() => FakeEventSource.last?.emit(type, data));

describe("Status view", () => {
  test("renders before the first Status arrives", async () => {
    await renderWith(null);
    expect(screen.getByRole("heading", { level: 1, name: "Status" })).toBeTruthy();
  });

  test("renders the initial Status without errors", async () => {
    await renderWith(initialStatus);
    expect(screen.getByText("No device workload")).toBeTruthy();
    expect(screen.getByText("Waiting for the printer")).toBeTruthy();
    expect(screen.getByText("Waiting for the scanner")).toBeTruthy();
    expect(screen.getByText("No config file")).toBeTruthy();
    expect(screen.getByText("No printer chosen yet")).toBeTruthy();
    expect(screen.queryByText("Simulated device")).toBeNull();
    expect(screen.queryByRole("alert")).toBeNull();
    // The zero changedAt is not a real date.
    expect(screen.queryByText(/^Changed /)).toBeNull();
    expect(screen.getByRole("heading", { name: "Connect a device" })).toBeTruthy();
    expect(screen.getAllByText("Not available yet").length).toBe(2);
    expect(screen.getByText("Not advertised yet")).toBeTruthy();
  });

  test("shows a device error prominently", async () => {
    await renderWith({
      ...initialStatus,
      device: {
        ...initialStatus.device,
        phase: "error",
        message: "device.mode usb needs a USB resource (set --usb-resource)",
      },
    });
    const alert = screen.getByRole("alert");
    expect(alert.textContent).toContain("device.mode usb needs a USB resource");
    expect(alert.textContent).toContain("The device has a problem");
  });

  test("shows the simulated decision, services, supplies and connect info", async () => {
    await renderWith(simulatedReady);
    expect(screen.getByText("Simulated device")).toBeTruthy();
    expect(screen.getByText("Simulated printer")).toBeTruthy();
    expect(screen.getByText("Discoverable on your network")).toBeTruthy();
    expect(screen.getByText("Out of paper")).toBeTruthy();
    expect(screen.getByText("Some new thing")).toBeTruthy();
    expect(screen.getByRole("meter", { name: "Black toner" })).toBeTruthy();
    expect(screen.getByText("level unknown")).toBeTruthy();
    expect(screen.queryByRole("alert")).toBeNull();
    expect(screen.getByText("ipp://192.168.1.20:8631/ipp/print")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Copy http://192.168.1.20:8090/eSCL" })).toBeTruthy();
  });

  test("tolerates repeated reasons, unnamed markers and duplicate services", async () => {
    await renderWith({
      ...simulatedReady,
      device: {
        ...simulatedReady.device,
        advertised: [
          { type: "_ipp._tcp", name: "Living room printer", port: 8631 },
          { type: "_ipp._tcp", name: "Living room printer", port: 8631 },
        ],
      },
      printer: {
        ...simulatedReady.printer,
        reasons: ["media-empty-error", "media-empty-warning"],
        markers: [
          { name: "", color: "#00ffff", level: 10 },
          { name: "", color: "#ff00ff", level: 20 },
        ],
      },
    });
    expect(screen.getAllByText("Out of paper").length).toBe(1);
    expect(screen.getAllByRole("meter", { name: "Supply" }).length).toBe(2);
  });

  test("shows the matched USB device, blobs, config errors and reconcile problems", async () => {
    await renderWith({
      ...initialStatus,
      config: {
        state: "invalid",
        etag: "e2",
        appliedEtag: "e1",
        errors: [{ path: "/device/mode", line: 4, column: 9, message: "must be usb or simulated" }],
        changedAt: "2026-11-24T17:00:00.123+01:00",
      },
      device: {
        ...initialStatus.device,
        phase: "degraded",
        mode: "usb",
        message: "HP plug-in: download failed",
        decision: {
          printer: "hplip",
          scanner: "airsane",
          saneBackends: ["hpaio"],
          requiredEulas: [],
          optionalEulas: ["hplip-plugin"],
          rule: "hp",
          device: {
            vendorId: "03f0",
            productId: "2b17",
            serial: "CN123",
            manufacturer: "HP",
            product: "LaserJet 1020",
            bus: 1,
            address: 4,
            interfaces: [{ class: 7, subClass: 1, protocol: 2 }],
            deviceId: "MFG:HP;MDL:LaserJet 1020;",
          },
        },
        blobs: [
          { id: "hplip-plugin", version: "3.24.4", state: "failed", message: "bad signature" },
        ],
      },
      reconcile: [
        {
          item: "device",
          ok: false,
          message: "apply failed: forbidden",
          at: "2026-11-24T17:00:00Z",
        },
        { item: "data-dir", ok: true, message: "", at: "2026-11-24T17:00:00Z" },
      ],
    });
    expect(screen.getByText("HP LaserJet 1020")).toBeTruthy();
    expect(screen.getByText(/USB 03f0:2b17/)).toBeTruthy();
    expect(screen.getByText("HP plug-in: download failed")).toBeTruthy();
    expect(screen.getByText("HP (HPLIP)")).toBeTruthy();
    const blob = screen.getByText(/hplip-plugin 3\.24\.4/).closest("li") as HTMLElement;
    expect(within(blob).getByText("Failed")).toBeTruthy();
    expect(within(blob).getByText("bad signature")).toBeTruthy();
    expect(screen.getByText("Has errors")).toBeTruthy();
    expect(screen.getByText("Line 4, column 9: must be usb or simulated")).toBeTruthy();
    expect(screen.getByText(/^Changed /)).toBeTruthy();
    expect(screen.getByRole("link", { name: "Open Settings" }).getAttribute("href")).toBe(
      "/settings",
    );
    expect(screen.getByText("apply failed: forbidden")).toBeTruthy();
    expect(screen.queryByText("data-dir")).toBeNull();
  });

  test("a job row moves from pending to completed after a job event", async () => {
    const job: Job = {
      id: 7,
      name: "boarding-pass.pdf",
      state: "pending",
      reasons: [],
      createdAt: "2026-11-24T17:00:00+01:00",
    };
    globalThis.fetch = jobsResponse([job]);
    await renderWith(simulatedReady);
    const row = await waitFor(
      () => screen.getByText("boarding-pass.pdf").closest("li") as HTMLElement,
    );
    expect(within(row).getByText("Waiting")).toBeTruthy();
    emit("job", { ...job, state: "completed" });
    await waitFor(() =>
      expect(
        within(screen.getByText("boarding-pass.pdf").closest("li") as HTMLElement).getByText(
          "Printed",
        ),
      ).toBeTruthy(),
    );
    expect(screen.getAllByText("boarding-pass.pdf").length).toBe(1);
  });
});
