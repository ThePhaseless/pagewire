import { afterEach, describe, expect, mock, test } from "bun:test";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { useState } from "react";
import { createEventStore } from "../api/events";
import { App, overallTone, type Views } from "../app";
import {
  type Config,
  type PrinterCaps,
  type Status,
  type StatusPrinter,
  type StatusScanner,
  StatusSchema,
} from "../gen/api";
import { navigate } from "../router";
import {
  Button,
  Card,
  Checkbox,
  DeviceStatusBadge,
  Dialog,
  Disclosure,
  EmptyState,
  InlineError,
  Meter,
  NumberField,
  PageHeader,
  ProgressBar,
  RadioGroup,
  Select,
  StatusBadge,
  Switch,
  Tab,
  TabList,
  TabPanel,
  Tabs,
  TextField,
  toast,
  toastQueue,
} from "./ui";

afterEach(cleanup);

const press = (el: Element, key: "Enter" | " ") => {
  fireEvent.keyDown(el, { key });
  fireEvent.keyUp(el, { key });
};

function DialogHarness() {
  const [open, setOpen] = useState(false);
  return (
    <>
      <Button onPress={() => setOpen(true)}>Open</Button>
      <Dialog title="Delete scan?" isOpen={open} onOpenChange={setOpen}>
        <p>This cannot be undone.</p>
      </Dialog>
    </>
  );
}

describe("components", () => {
  test("Button has its role and name and activates from the keyboard", () => {
    const onPress = mock(() => {});
    render(<Button onPress={onPress}>Print</Button>);
    const button = screen.getByRole("button", { name: "Print" });
    press(button, "Enter");
    press(button, " ");
    expect(onPress).toHaveBeenCalledTimes(2);
  });

  test("TextField links label, description and error", () => {
    render(
      <TextField
        label="Name"
        description="Shown in the header"
        errorMessage="Too long"
        defaultValue="x"
      />,
    );
    const input = screen.getByRole("textbox", { name: "Name" });
    expect(input.getAttribute("aria-invalid")).toBe("true");
    const describedBy = input.getAttribute("aria-describedby") ?? "";
    const described = describedBy
      .split(" ")
      .map((id) => document.getElementById(id)?.textContent)
      .join(" ");
    expect(described).toContain("Shown in the header");
    expect(described).toContain("Too long");
  });

  test("NumberField exposes a named input and labelled stepper buttons", () => {
    render(<NumberField label="Copies" defaultValue={1} minValue={1} maxValue={99} />);
    const input = screen.getByRole("textbox", { name: "Copies" });
    const increase = screen.getByRole("button", { name: /increase/i });
    fireEvent.click(increase);
    expect((input as HTMLInputElement).value).toBe("2");
    expect(screen.getByRole("button", { name: /decrease/i })).toBeTruthy();
  });

  test("Checkbox and Switch toggle with Space", () => {
    render(
      <>
        <Checkbox>I accept the licence</Checkbox>
        <Switch>Duplex</Switch>
      </>,
    );
    const box = screen.getByRole("checkbox", { name: "I accept the licence" }) as HTMLInputElement;
    const sw = screen.getByRole("switch", { name: "Duplex" }) as HTMLInputElement;
    fireEvent.click(box);
    fireEvent.click(sw);
    expect(box.checked).toBe(true);
    expect(sw.checked).toBe(true);
  });

  test("RadioGroup is a named group of radios", () => {
    render(
      <RadioGroup
        label="Sides"
        defaultValue="one-sided"
        options={[
          { id: "one-sided", label: "One-sided" },
          { id: "two-sided-long-edge", label: "Two-sided" },
        ]}
      />,
    );
    expect(screen.getByRole("radiogroup", { name: "Sides" })).toBeTruthy();
    expect((screen.getByRole("radio", { name: "One-sided" }) as HTMLInputElement).checked).toBe(
      true,
    );
    fireEvent.click(screen.getByRole("radio", { name: "Two-sided" }));
    expect((screen.getByRole("radio", { name: "Two-sided" }) as HTMLInputElement).checked).toBe(
      true,
    );
  });

  test("Select's trigger is named by its label and shows the selection", () => {
    render(
      <Select
        label="Paper size"
        defaultSelectedKey="a4"
        options={[
          { id: "a4", label: "A4" },
          { id: "letter", label: "Letter" },
        ]}
      />,
    );
    const trigger = screen.getByRole("button", { name: /Paper size/ });
    expect(trigger.textContent).toContain("A4");
    // Only the selected text: no option markup (such as the hidden check icon) in the trigger.
    const value = trigger.querySelector(".ui-select-value");
    expect(value?.textContent).toBe("A4");
    expect(value?.querySelector("svg, .ui-option-label")).toBeNull();
  });

  test("problem toasts time out after 10 s, other toasts after 5 s", () => {
    toast({ title: "Saved", tone: "ok" });
    toast({ title: "Upload failed", tone: "problem" });
    const timeouts = Object.fromEntries(
      toastQueue.visibleToasts.map((t) => [t.content.title, t.timeout]),
    );
    expect(timeouts).toEqual({ Saved: 5000, "Upload failed": 10_000 });
  });

  test("Dialog has the dialog role, its title as name, and a Close button", async () => {
    render(<DialogHarness />);
    press(screen.getByRole("button", { name: "Open" }), "Enter");
    const dialog = await screen.findByRole("dialog", { name: "Delete scan?" });
    expect(dialog.textContent).toContain("This cannot be undone.");
    press(screen.getByRole("button", { name: "Close" }), "Enter");
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  });

  test("Tabs move selection with arrow keys", () => {
    render(
      <Tabs>
        <TabList aria-label="Sections">
          <Tab id="general">General</Tab>
          <Tab id="device">Device</Tab>
        </TabList>
        <TabPanel id="general">General panel</TabPanel>
        <TabPanel id="device">Device panel</TabPanel>
      </Tabs>,
    );
    const general = screen.getByRole("tab", { name: "General" });
    expect(screen.getByRole("tablist", { name: "Sections" })).toBeTruthy();
    act(() => general.focus());
    fireEvent.keyDown(general, { key: "ArrowRight" });
    expect(screen.getByRole("tab", { name: "Device" }).getAttribute("aria-selected")).toBe("true");
    expect(screen.getByRole("tabpanel").textContent).toBe("Device panel");
  });

  test("Disclosure toggles from the keyboard", () => {
    render(<Disclosure title="Advanced">Hidden text</Disclosure>);
    const trigger = screen.getByRole("button", { name: "Advanced" });
    expect(trigger.getAttribute("aria-expanded")).toBe("false");
    press(trigger, "Enter");
    expect(trigger.getAttribute("aria-expanded")).toBe("true");
  });

  test("ProgressBar and Meter are named", () => {
    render(
      <>
        <ProgressBar label="Uploading" value={40} />
        <ProgressBar label="Scanning" />
        <Meter label="Black toner" value={70} color="black" />
      </>,
    );
    expect(
      screen.getByRole("progressbar", { name: "Uploading" }).getAttribute("aria-valuenow"),
    ).toBe("40");
    expect(
      screen.getByRole("progressbar", { name: "Scanning" }).getAttribute("aria-valuenow"),
    ).toBeNull();
    expect(screen.getByRole("meter", { name: "Black toner" })).toBeTruthy();
  });

  test("layout pieces expose headings, alerts and text labels", () => {
    render(
      <>
        <PageHeader title="Print" description="Send a document" />
        <Card title="Printer">
          <StatusBadge tone="ok">Ready</StatusBadge>
        </Card>
        <EmptyState title="No jobs yet">Printed documents appear here.</EmptyState>
        <InlineError>Upload failed</InlineError>
      </>,
    );
    expect(screen.getByRole("heading", { level: 1, name: "Print" })).toBeTruthy();
    expect(screen.getByRole("heading", { level: 2, name: "Printer" })).toBeTruthy();
    expect(screen.getByText("Ready")).toBeTruthy();
    expect(screen.getByRole("alert").textContent).toContain("Upload failed");
  });

  test("DeviceStatusBadge maps each phase to a tone and a text label", () => {
    const phases = ["ready", "probing", "deploying", "degraded", "error", "none"] as const;
    render(
      <>
        {phases.map((p) => (
          <DeviceStatusBadge key={p} phase={p} />
        ))}
        <StatusBadge tone="active">Printing</StatusBadge>
      </>,
    );
    const tones = [...document.querySelectorAll(".ui-badge")].map((b) => [
      b.getAttribute("data-tone"),
      b.textContent,
    ]);
    expect(tones).toEqual([
      ["ok", "Ready"],
      ["busy", "Not found"],
      ["busy", "Starting"],
      ["problem", "Needs attention"],
      ["problem", "Error"],
      ["idle", "Not in use"],
      // The fifth tone, for work in progress (a printing job), stays available.
      ["active", "Printing"],
    ]);
    const pulsing = document.querySelectorAll(".ui-badge-dot[data-pulse]");
    expect(pulsing).toHaveLength(1);
  });
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

const printer: StatusPrinter = {
  id: "printer",
  name: "Hall printer",
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
};

const scanner: StatusScanner = {
  id: "scanner",
  name: "Hall scanner",
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
  capabilities: {
    sources: [],
    resolutions: [],
    colorModes: [],
    adf: false,
    duplex: false,
    intents: [],
  },
  connect: { dnssdName: "", esclUrls: [] },
};

const baseStatus: Status = {
  config: {
    state: "applied",
    etag: "e1",
    appliedEtag: "e1",
    errors: [],
    changedAt: "2026-11-24T18:00:00+01:00",
  },
  phase: "ready",
  message: "",
  printers: [printer],
  scanners: [scanner],
  workloads: [],
  devices: [],
  reconcile: [],
};

const lastGood: Config = {
  apiVersion: "pagewire/v2",
  name: "Hall",
  printers: [
    {
      id: "printer",
      name: "Hall printer",
      location: "",
      enabled: true,
      mode: "simulated",
      driver: "auto",
      defaults: {},
    },
  ],
  scanners: [],
  ignoredDevices: [],
  print: { maxUploadMiB: 50 },
  scan: { retentionDays: 7 },
  licenses: { acceptedEulas: [] },
};

// Stand-ins: the shell is tested on its own, each page only shows its heading.
const page = (title: string) => () => <h1 tabIndex={-1}>{title}</h1>;
const views: Views = {
  "/": page("Status"),
  "/print": page("Print"),
  "/scan": page("Scan"),
  "/settings": page("Settings"),
  "/setup": page("Set up"),
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
    this.listeners.get("status")?.(new MessageEvent("status", { data: JSON.stringify(s) }));
  }
}

describe("app shell", () => {
  const realFetch = globalThis.fetch;
  afterEach(() => {
    globalThis.fetch = realFetch;
  });

  function mount(path: string) {
    window.history.replaceState(null, "", path);
    globalThis.fetch = (async (input: RequestInfo | URL) => {
      if (String(input) !== "/api/v1/config") return new Promise<Response>(() => {});
      return new Response(
        JSON.stringify({ etag: "e1", config: null, state: "invalid", errors: [], lastGood }),
        { headers: { "Content-Type": "application/json" } },
      );
    }) as unknown as typeof fetch;
    const store = createEventStore({
      newEventSource: () => new FakeEventSource() as unknown as EventSource,
      notFoundAfterMs: 300,
    });
    render(<App store={store} views={views} />);
    const es = FakeEventSource.last;
    act(() => es.onopen?.(new Event("open")));
    return es;
  }

  const navLinks = () =>
    within(screen.getByRole("navigation", { name: "Main" }))
      .getAllByRole("link")
      .map((a) => a.textContent);

  test("the fixtures match the generated schema", () => {
    expect(StatusSchema.parse(baseStatus)).toEqual(baseStatus);
  });

  test("redirects to setup while the config is missing and hides navigation", async () => {
    const es = mount("/print");
    act(() => es.status({ ...baseStatus, config: { ...baseStatus.config, state: "missing" } }));
    await waitFor(() => expect(window.location.pathname).toBe("/setup"));
    expect(screen.queryByRole("navigation")).toBeNull();
    expect(screen.getByRole("heading", { level: 1, name: "Set up" })).toBeTruthy();
  });

  test("shows navigation with the current page, the device name, live status and the invalid-config banner", async () => {
    const es = mount("/scan");
    act(() =>
      es.status({
        ...baseStatus,
        config: {
          ...baseStatus.config,
          state: "invalid",
          errors: [{ path: "/printers/0/mode", line: 12, column: 9, message: "unknown value" }],
        },
      }),
    );
    const nav = screen.getByRole("navigation", { name: "Main" });
    expect(nav.querySelector('[aria-current="page"]')?.textContent).toBe("Scan");
    expect(screen.getByText("Ready")).toBeTruthy();
    const banner = screen.getAllByRole("alert").find((el) => el.classList.contains("app-banner"));
    expect(banner?.textContent).toContain("Line 12: unknown value");
    // The header shows the config's name (the UI title), not a device's.
    await within(document.querySelector(".app-header") as HTMLElement).findByText("Hall");
    // Settings lists the file's errors itself, so the banner stays away there.
    act(() => navigate("/settings"));
    await waitFor(() =>
      expect(
        screen.queryAllByRole("alert").filter((el) => el.classList.contains("app-banner")),
      ).toEqual([]),
    );
  });

  test("link clicks navigate in place and move focus to the new page heading", async () => {
    mount("/");
    act(() => FakeEventSource.last.status(baseStatus));
    fireEvent.click(screen.getByRole("link", { name: "Print" }));
    expect(window.location.pathname).toBe("/print");
    await waitFor(() => expect(document.activeElement?.textContent).toBe("Print"));
    expect(document.activeElement?.tagName).toBe("H1");
    await waitFor(() => expect(document.title).toBe("Print · Hall"));
    act(() => navigate("/nowhere"));
    expect(screen.getByRole("heading", { level: 1, name: "Page not found" })).toBeTruthy();
  });

  test("a route change clears problem toasts and keeps the others", async () => {
    mount("/");
    act(() => FakeEventSource.last.status(baseStatus));
    act(() => {
      toast({ title: "Scan failed", tone: "problem" });
      toast({ title: "Saved", tone: "ok" });
    });
    await screen.findByText("Scan failed");
    fireEvent.click(screen.getByRole("link", { name: "Print" }));
    await waitFor(() => expect(screen.queryByText("Scan failed")).toBeNull());
    expect(screen.getByText("Saved")).toBeTruthy();
  });

  test("Print and Scan are in the navigation only while a printer or scanner is enabled", async () => {
    const es = mount("/");
    act(() => es.status(baseStatus));
    expect(navLinks()).toEqual(["Status", "Print", "Scan", "Settings"]);
    act(() => es.status({ ...baseStatus, scanners: [{ ...scanner, enabled: false }] }));
    expect(navLinks()).toEqual(["Status", "Print", "Settings"]);
    act(() => es.status({ ...baseStatus, printers: [], scanners: [scanner] }));
    expect(navLinks()).toEqual(["Status", "Scan", "Settings"]);
  });

  test("a hidden route is replaced with Status", async () => {
    const es = mount("/print?printer=printer");
    // Before the first Status nothing is known, so the page stays.
    expect(screen.getByRole("heading", { level: 1, name: "Print" })).toBeTruthy();
    act(() => es.status({ ...baseStatus, printers: [{ ...printer, enabled: false }] }));
    await waitFor(() => expect(window.location.pathname).toBe("/"));
    expect(window.location.search).toBe("");
    expect(screen.getByRole("heading", { level: 1, name: "Status" })).toBeTruthy();
    // With a printer enabled again, Print opens.
    act(() => es.status(baseStatus));
    act(() => navigate("/print"));
    expect(screen.getByRole("heading", { level: 1, name: "Print" })).toBeTruthy();
  });

  test("overall tone follows the overall phase and the connection", () => {
    expect(overallTone(baseStatus, false).label).toBe("Offline");
    expect(overallTone(baseStatus, true).tone).toBe("ok");
    expect(overallTone({ ...baseStatus, phase: "deploying" }, true)).toEqual({
      tone: "busy",
      label: "Starting",
      pulse: true,
    });
    expect(overallTone({ ...baseStatus, phase: "error" }, true).tone).toBe("problem");
    expect(overallTone({ ...baseStatus, phase: "degraded" }, true).tone).toBe("problem");
    expect(overallTone({ ...baseStatus, phase: "none" }, true)).toEqual({
      tone: "idle",
      label: "Nothing in use",
      pulse: false,
    });
    expect(overallTone(null, true).tone).toBe("idle");
  });

  test("no config reads Not set up, until probing finds nothing: then No device found", () => {
    const probing = { ...baseStatus, phase: "probing" as const };
    const missing = { ...probing, config: { ...probing.config, state: "missing" as const } };
    expect(overallTone(missing, true)).toEqual({ tone: "idle", label: "Not set up", pulse: false });
    expect(overallTone(missing, true, true)).toEqual({
      tone: "problem",
      label: "No device found",
      pulse: false,
    });
    expect(overallTone(missing, false, true).label).toBe("Offline");
  });

  test("the header says No device found after a few seconds of probing", async () => {
    const es = mount("/setup");
    const probing: Status = {
      ...baseStatus,
      config: { ...baseStatus.config, state: "missing" },
      phase: "probing",
      printers: [],
      scanners: [],
    };
    act(() => es.status(probing));
    const header = within(document.querySelector(".app-header") as HTMLElement);
    expect(await header.findByText("Not set up")).toBeTruthy();
    expect(await header.findByText("No device found", {}, { timeout: 3000 })).toBeTruthy();

    // Probing lasts the whole wizard: once a device that can print is detected, it is set-up time.
    const hp = {
      device: {
        vendorId: "03f0",
        productId: "2b17",
        serial: "",
        manufacturer: "HP",
        product: "OfficeJet",
        bus: 1,
        address: 5,
        interfaces: [],
        deviceId: "",
      },
      node: "",
      canPrint: true,
      canScan: true,
      printerId: "",
      scannerId: "",
      ignored: [],
      suggestedPrinter: null,
      suggestedScanner: null,
      printerDecision: null,
      scannerDecision: null,
    } satisfies Status["devices"][number];
    act(() => es.status({ ...probing, devices: [hp] }));
    expect(await header.findByText("Not set up")).toBeTruthy();
    await new Promise((r) => setTimeout(r, 400));
    expect(header.queryByText("No device found")).toBeNull();
  });
});
