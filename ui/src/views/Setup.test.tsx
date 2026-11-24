import { afterEach, beforeEach, describe, expect, mock, test } from "bun:test";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { createEventStore, type EventStore, EventsContext } from "../api/events";
import { EulaAccept } from "../components/EulaAccept";
import type { Config, Setup as SetupDTO, SetupEULA, Status } from "../gen/api";
import { mediaForLanguage, Setup } from "./Setup";

// The Status the controller returns before any input (plan: controller rules, "Initial Status").
const initialStatus: Status = {
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

const hpDevice = {
  vendorId: "03f0",
  productId: "042a",
  serial: "000000000QH7B4KQPR1a",
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
const hpInventory = {
  devices: [hpDevice],
  probedAt: "2026-11-24T12:00:00+01:00",
  simulated: false,
};
const hpDecision = {
  printer: "hplip",
  scanner: "airsane",
  saneBackends: ["hpaio"],
  requiredEulas: [],
  optionalEulas: ["hplip-plugin"],
  rule: "hp",
  device: hpDevice,
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
const hplipEula: SetupEULA = {
  id: "hplip-plugin",
  version: "3.24.4",
  title: "HP Linux Imaging and Printing proprietary plug-in",
  vendor: "HP",
  url: "https://developers.hp.com/hp-linux-imaging-and-printing/plugins",
  summary: "Needed by some HP lasers for firmware upload and by some HP scanners.",
  required: false,
};

function suggested(name: string, mode: "usb" | "simulated"): Config {
  return {
    apiVersion: "printscan/v1",
    name,
    location: "",
    device: { mode, driver: "auto", scanner: "auto" },
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
}

const hpSetup: SetupDTO = {
  required: true,
  backend: "k8s",
  usbResourceConfigured: true,
  inventory: hpInventory,
  decision: hpDecision,
  eulas: [hplipEula],
  suggested: suggested("HP LaserJet Professional M1132 MFP", "usb"),
};
const noUsbSetup: SetupDTO = {
  required: true,
  backend: "sim",
  usbResourceConfigured: false,
  inventory: null,
  decision: null,
  eulas: [],
  suggested: suggested("Printer", "simulated"),
};
const waitingSetup: SetupDTO = {
  ...hpSetup,
  inventory: null,
  decision: null,
  eulas: [],
  suggested: suggested("Printer", "usb"),
};

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
const realFetch = globalThis.fetch;
const json = (body: unknown, status = 200) =>
  new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

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
    if (url.endsWith("/api/v1/config") && method === "PUT") return putResponse();
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

const press = (name: string | RegExp) => {
  const el = screen.getByRole("button", { name });
  fireEvent.keyDown(el, { key: "Enter" });
  fireEvent.keyUp(el, { key: "Enter" });
};
const heading = (name: string | RegExp) => screen.findByRole("heading", { level: 1, name });

beforeEach(() => {
  window.history.replaceState(null, "", "/setup");
  putResponse = () =>
    json(
      {
        etag: "e1",
        config: (calls.at(-1)?.body as Config) ?? null,
        state: "applied",
        errors: [],
        lastGood: null,
      },
      201,
    );
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
  test("starts unchecked, names the licence, and links to it in a new tab", () => {
    const onChange = mock((_: boolean) => {});
    render(<EulaAccept eula={hplipEula} isAccepted={false} onChange={onChange} />);
    const box = screen.getByRole("checkbox", {
      name: "I have read and accept the HP Linux Imaging and Printing proprietary plug-in licence",
    }) as HTMLInputElement;
    expect(box.checked).toBe(false);
    const link = screen.getByRole("link", { name: /opens in a new tab/ });
    expect(link.getAttribute("href")).toBe(hplipEula.url);
    expect(link.getAttribute("target")).toBe("_blank");
    expect(document.body.textContent).toContain("Nothing is downloaded otherwise");
    expect(document.body.textContent).toContain("HP, version 3.24.4");
    fireEvent.click(box);
    expect(onChange).toHaveBeenCalledWith(true);
  });

  test("an accepted licence can be revoked", () => {
    const onChange = mock((_: boolean) => {});
    render(
      <EulaAccept
        eula={{ ...hplipEula, url: "javascript:alert(1)" }}
        isAccepted
        onChange={onChange}
      />,
    );
    const box = screen.getByRole("checkbox") as HTMLInputElement;
    expect(box.checked).toBe(true);
    expect(screen.queryByRole("link")).toBeNull();
    fireEvent.click(box);
    expect(onChange).toHaveBeenCalledWith(false);
  });
});

describe("Setup wizard", () => {
  test("fixture path: detect, name, licence, defaults, review, then ready with connect info", async () => {
    mockServer(() => hpSetup);
    mount();
    await heading("Find the printer");
    expect(await screen.findByText("HP LaserJet Professional M1132 MFP")).toBeTruthy();
    expect(document.body.textContent).toContain("USB 03f0:042a");
    expect(document.body.textContent).toContain("HP (HPLIP)");
    expect(screen.getByRole("list", { name: "Setup steps" })).toBeTruthy();
    expect(document.querySelector('[aria-current="step"]')?.textContent).toContain(
      "Find the printer",
    );

    press("Next");
    await heading("Name and location");
    expect(window.location.hash).toBe("#name");
    const name = screen.getByRole("textbox", { name: "Name" }) as HTMLInputElement;
    expect(name.value).toBe("HP LaserJet Professional M1132 MFP");
    fireEvent.change(name, { target: { value: "" } });
    press("Next");
    expect(await screen.findByText(/Enter 1 to 63 characters/)).toBeTruthy();
    expect(window.location.hash).toBe("#name");
    fireEvent.change(name, { target: { value: "Hall printer" } });
    fireEvent.change(screen.getByRole("textbox", { name: "Location" }), {
      target: { value: "Hall" },
    });
    press("Next");

    await heading("Driver and licences");
    expect(document.body.textContent).toContain("“hp”");
    const box = screen.getByRole("checkbox", {
      name: /accept the HP Linux Imaging/,
    }) as HTMLInputElement;
    expect(box.checked).toBe(false);
    fireEvent.click(box);
    press("Next");

    await heading("Defaults");
    // happy-dom reports en-US, so Letter is preselected.
    expect(screen.getByRole("button", { name: /Paper size/ }).textContent).toContain("Letter");
    // Pick other values through the Select popovers; they must reach the PUT body.
    fireEvent.click(screen.getByRole("button", { name: /Paper size/ }));
    fireEvent.click(await screen.findByRole("option", { name: "A4" }));
    await waitFor(() =>
      expect(screen.getByRole("button", { name: /Paper size/ }).textContent).toContain("A4"),
    );
    fireEvent.click(screen.getByRole("button", { name: /Resolution/ }));
    fireEvent.click(await screen.findByRole("option", { name: "600 dpi" }));
    await waitFor(() =>
      expect(screen.getByRole("button", { name: /Resolution/ }).textContent).toContain("600 dpi"),
    );
    press("Next");

    await heading("Review and finish");
    // Back keeps the edits.
    press("Back");
    await heading("Defaults");
    press("Next");
    await heading("Review and finish");
    expect(document.body.textContent).toContain("Hall printer");
    press("Finish setup");

    await heading("Starting the printer");
    const put = calls.find((c) => c.method === "PUT");
    expect(put?.headers["If-None-Match"]).toBe("*");
    const body = put?.body as Config;
    expect(body.name).toBe("Hall printer");
    expect(body.location).toBe("Hall");
    expect(body.device).toEqual({ mode: "usb", driver: "auto", scanner: "auto" });
    expect(body.print.defaults.media).toBe("iso_a4_210x297mm");
    expect(body.scan.defaults.resolution).toBe(600);
    expect(body.licenses.acceptedEulas).toEqual([{ id: "hplip-plugin", version: "3.24.4" }]);

    FakeEventSource.last.status({
      ...initialStatus,
      config: { ...initialStatus.config, state: "applied", etag: "e1", appliedEtag: "e1" },
      device: {
        ...initialStatus.device,
        phase: "deploying",
        mode: "usb",
        message: "Waiting for the device agent",
      },
    });
    expect(await screen.findByText("Waiting for the device agent")).toBeTruthy();
    FakeEventSource.last.status({
      ...initialStatus,
      config: { ...initialStatus.config, state: "applied", etag: "e1", appliedEtag: "e1" },
      device: {
        ...initialStatus.device,
        phase: "ready",
        mode: "usb",
        decision: hpDecision,
        inventory: hpInventory,
      },
      connect: {
        dnssdName: "Hall printer",
        ippUris: ["ipp://192.168.1.20:8002/ipp/print/Hall_printer"],
        esclUrls: ["http://192.168.1.20:8090/eSCL"],
      },
    });
    await heading("printscan is ready");
    expect(document.body.textContent).toContain("ipp://192.168.1.20:8002/ipp/print/Hall_printer");
    expect(document.body.textContent).toContain("http://192.168.1.20:8090/eSCL");
    expect(screen.getByRole("link", { name: "Go to Status" }).getAttribute("href")).toBe("/");
  });

  test("no-USB path: prerequisites, simulated printer, ready with a simulated decision", async () => {
    mockServer(() => noUsbSetup);
    mount();
    await heading("Find the printer");
    expect(await screen.findByText("No USB printer available")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Next" }).hasAttribute("disabled")).toBe(true);
    press("Use a simulated printer");

    await heading("Name and location");
    press("Next");
    await heading("Driver and licences");
    expect(document.body.textContent).toContain("need no driver");
    press("Next");
    await heading("Defaults");
    press("Next");
    await heading("Review and finish");
    expect(document.body.textContent).toContain("Simulated printer and scanner");
    press("Finish setup");

    await heading("Starting the printer");
    const body = calls.find((c) => c.method === "PUT")?.body as Config;
    expect(body.device).toEqual({ mode: "simulated", driver: "auto", scanner: "auto" });
    expect(body.name).toBe("Printer");

    FakeEventSource.last.status({
      ...initialStatus,
      config: { ...initialStatus.config, state: "applied", etag: "e1", appliedEtag: "e1" },
      device: {
        ...initialStatus.device,
        phase: "ready",
        mode: "simulated",
        decision: simulatedDecision,
        inventory: {
          devices: [],
          probedAt: "2026-11-24T12:00:00.123456789+01:00",
          simulated: true,
        },
      },
      connect: { dnssdName: "Printer", ippUris: ["ipp://127.0.0.1:8631/ipp/print"], esclUrls: [] },
    });
    await heading("printscan is ready");
    expect(document.body.textContent).toContain("ipp://127.0.0.1:8631/ipp/print");
  });

  test("Detect shows the unschedulable-probe message and updates when the probe reports", async () => {
    let current = waitingSetup;
    mockServer(() => current);
    mount();
    await heading("Find the printer");
    FakeEventSource.last.status({
      ...initialStatus,
      device: {
        ...initialStatus.device,
        phase: "probing",
        mode: "probe",
        message: "Printer not plugged in or not visible to the device plugin",
      },
    });
    expect(
      await screen.findByText("Printer not plugged in or not visible to the device plugin"),
    ).toBeTruthy();
    expect(document.body.textContent).toContain("Plug the printer in");
    expect(screen.getByRole("button", { name: "Next" }).hasAttribute("disabled")).toBe(true);

    current = hpSetup;
    FakeEventSource.last.status({
      ...initialStatus,
      device: { ...initialStatus.device, phase: "probing", mode: "probe", inventory: hpInventory },
    });
    expect(await screen.findByText("HP LaserJet Professional M1132 MFP")).toBeTruthy();
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Next" }).hasAttribute("disabled")).toBe(false),
    );
    press("Next");
    await heading("Name and location");
    // The suggested name followed the detected device.
    expect((screen.getByRole("textbox", { name: "Name" }) as HTMLInputElement).value).toBe(
      "HP LaserJet Professional M1132 MFP",
    );
  });

  test("a 412 says another session finished setup and reloads", async () => {
    mockServer(() => noUsbSetup);
    putResponse = () =>
      json(
        {
          etag: "x",
          config: suggested("Other", "simulated"),
          state: "applied",
          errors: [],
          lastGood: null,
        },
        412,
      );
    const reload = mock(() => {});
    mount({ reload, reloadDelayMs: 10 });
    await heading("Find the printer");
    press("Use a simulated printer");
    for (const h of ["Name and location", "Driver and licences", "Defaults"]) {
      await heading(h);
      press("Next");
    }
    await heading("Review and finish");
    press("Finish setup");
    expect(await screen.findByText("Another session finished setup. Reloading…")).toBeTruthy();
    await waitFor(() => expect(reload).toHaveBeenCalledTimes(1));
  });

  test("waiting for ready times out with a message", async () => {
    mockServer(() => noUsbSetup);
    mount({ readyTimeoutMs: 20 });
    await heading("Find the printer");
    press("Use a simulated printer");
    for (const h of ["Name and location", "Driver and licences", "Defaults"]) {
      await heading(h);
      press("Next");
    }
    await heading("Review and finish");
    press("Finish setup");
    await heading("Starting the printer");
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
          errors: [{ path: "/name", line: 2, column: 7, message: "too long" }],
        },
        422,
      );
    mount();
    await heading("Find the printer");
    press("Use a simulated printer");
    for (const h of ["Name and location", "Driver and licences", "Defaults"]) {
      await heading(h);
      press("Next");
    }
    await heading("Review and finish");
    press("Finish setup");
    expect((await screen.findByRole("alert")).textContent).toContain("/name: too long");
    expect(screen.getByRole("heading", { level: 1 }).textContent).toBe("Review and finish");
  });

  test("a reload on a later step starts again at Detect", async () => {
    window.history.replaceState(null, "", "/setup#review");
    mockServer(() => hpSetup);
    mount();
    await heading("Find the printer");
  });

  test("an existing config shows the already-set-up note", async () => {
    mockServer(() => ({ ...hpSetup, required: false }));
    mount();
    await heading("printscan is already set up");
    expect(screen.getByRole("link", { name: "Go to Settings" }).getAttribute("href")).toBe(
      "/settings",
    );
  });
});
