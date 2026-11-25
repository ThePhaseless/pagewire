import { afterEach, beforeEach, describe, expect, test } from "bun:test";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { createEventStore, type EventStore, EventsContext } from "../api/events";
import type { Config, ConfigResponse, Setup as SetupDTO, Status } from "../gen/api";
import { navigate } from "../router";
import { Settings } from "./Settings";

const SHA = "c".repeat(64);
const HPLIP = { id: "hplip-plugin", version: "3.24.4" };
const HPLIP_ACCEPTED = { ...HPLIP, textSha256: SHA };

function config(over: Partial<Config> = {}): Config {
  return {
    apiVersion: "printscan/v1",
    name: "Living room printer",
    location: "",
    device: {
      mode: "usb",
      usb: { vendorId: "03f0", productId: "2b17", serial: "" },
      driver: "auto",
      scanner: "auto",
    },
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
    licenses: { acceptedEulas: [HPLIP_ACCEPTED] },
    ...over,
  };
}

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
  inventory: null,
  decision: null,
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
  suggested: config(),
};

const status = (blobs: Status["device"]["blobs"]): Status => ({
  config: {
    state: "applied",
    etag: "e1",
    appliedEtag: "e1",
    errors: [],
    changedAt: "2026-11-24T12:00:00+01:00",
  },
  device: {
    phase: "degraded",
    mode: "usb",
    decision: null,
    inventory: null,
    message: "",
    advertised: [],
    blobs,
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

async function mount() {
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
}

const press = (el: HTMLElement) =>
  act(async () => {
    fireEvent.click(el);
  });
const button = (name: string | RegExp, scope: HTMLElement = document.body) =>
  within(scope).getByRole("button", { name });
const field = (name: string) => screen.getByRole("textbox", { name }) as HTMLInputElement;
const type = async (name: string, value: string) => {
  const el = await screen.findByRole("textbox", { name });
  act(() => {
    fireEvent.change(el, { target: { value } });
  });
};
const writesOf = (method: string) => calls.filter((c) => c.method === method);

describe("Settings form", () => {
  test("shows each section and sends edits as a merge patch with If-Match", async () => {
    await mount();
    for (const name of ["General", "Device", "Printing", "Scanning", "Licences"])
      expect(await screen.findByRole("heading", { level: 2, name })).toBeTruthy();
    expect((button("Save") as HTMLButtonElement).disabled).toBe(true);

    await type("Name", "Office");
    writes.push(() => json(applied("e2", config({ name: "Office" }))));
    await press(button("Save"));

    const [patch] = writesOf("PATCH");
    expect(patch?.body).toEqual({ name: "Office" });
    expect(patch?.headers["If-Match"]).toBe('"e1"');
    expect(patch?.headers["Content-Type"]).toBe("application/merge-patch+json");
    await waitFor(() => expect((button("Save") as HTMLButtonElement).disabled).toBe(true));
    expect(field("Name").value).toBe("Office");
  });

  test("turning off the USB selector sends null for device.usb", async () => {
    await mount();
    const sw = await screen.findByRole("switch", { name: "Use a specific USB device" });
    expect(field("Vendor ID").value).toBe("03f0");
    act(() => {
      fireEvent.click(sw);
    });
    expect(screen.queryByRole("textbox", { name: "Vendor ID" })).toBeNull();
    writes.push(() => json(applied("e2")));
    await press(button("Save"));
    expect(writesOf("PATCH")[0]?.body).toEqual({ device: { usb: null } });
  });

  test("switching to the simulated device also drops the USB selector", async () => {
    await mount();
    const radio = await screen.findByRole("radio", { name: "Simulated printer and scanner" });
    act(() => {
      fireEvent.click(radio);
    });
    expect(screen.queryByRole("switch")).toBeNull();
    writes.push(() => json(applied("e2")));
    await press(button("Save"));
    expect(writesOf("PATCH")[0]?.body).toEqual({ device: { mode: "simulated", usb: null } });
  });

  test("a 422 puts each error on its field and lists the rest", async () => {
    await mount();
    await type("Name", "Bad\u0007");
    writes.push(() =>
      problem(422, "about:blank", [
        { path: "/name", line: 2, column: 7, message: "must not contain control characters" },
        { path: "/nonsense", line: 9, column: 1, message: "something else" },
      ]),
    );
    await press(button("Save"));
    const nameField = field("Name");
    await waitFor(() => expect(nameField.getAttribute("aria-invalid")).toBe("true"));
    expect(document.body.textContent).toContain("must not contain control characters");
    const summary = screen.getByRole("alert");
    expect(summary.textContent).toContain("/nonsense: something else");
  });

  test("Reset discards edits", async () => {
    await mount();
    await type("Name", "Office");
    await press(button("Reset"));
    expect(field("Name").value).toBe("Living room printer");
  });

  test("unsaved edits keep Reset and Save in a sticky bar that says so", async () => {
    await mount();
    const bar = () => button("Save").closest(".settings-actions") as HTMLElement;
    expect(bar().hasAttribute("data-sticky")).toBe(false);
    expect(screen.queryByText("Unsaved changes")).toBeNull();
    await type("Name", "Office");
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
      await type("Name", "Office");
      expect(unload()).toBe(true);

      act(() => navigate("/print"));
      let dialog = await screen.findByRole("alertdialog", { name: "Leave without saving?" });
      expect(window.location.pathname).toBe("/settings");
      await press(button("Keep editing", dialog));
      expect(screen.queryByRole("alertdialog")).toBeNull();
      expect(field("Name").value).toBe("Office");

      act(() => navigate("/print"));
      dialog = await screen.findByRole("alertdialog", { name: "Leave without saving?" });
      await press(button("Leave", dialog));
      expect(window.location.pathname).toBe("/print");
    } finally {
      window.history.replaceState(null, "", "/");
    }
  });
});

describe("412 with a valid file", () => {
  const theirs = config({ name: "Kitchen", location: "Hall" });

  async function conflict() {
    await mount();
    await type("Name", "Office");
    writes.push(() => json(applied("e2", theirs), 412));
    await press(button("Save"));
    return screen.findByRole("dialog", { name: "Someone else changed the settings" });
  }

  test("lists each changed field and Keep mine re-sends the patch with the new ETag", async () => {
    const dialog = await conflict();
    const row = within(dialog).getAllByRole("listitem")[0] as HTMLElement;
    expect(row.textContent).toContain("Name");
    expect(row.textContent).toContain("Yours");
    expect(row.textContent).toContain("Office");
    expect(row.textContent).toContain("In the file now");
    expect(row.textContent).toContain("Kitchen");
    // Fields only the other edit changed are listed too.
    const also = within(dialog).getByRole("list", { name: "Also changed in the file" });
    expect(also.textContent).toContain("Location");
    expect(also.textContent).toContain("Hall");
    expect(also.textContent).not.toContain("Name");

    writes.push(() => json(applied("e3", { ...theirs, name: "Office" })));
    await press(button("Keep mine", dialog));
    const patches = writesOf("PATCH");
    expect(patches).toHaveLength(2);
    expect(patches[1]?.body).toEqual({ name: "Office" });
    expect(patches[1]?.headers["If-Match"]).toBe('"e2"');
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(field("Name").value).toBe("Office");
    expect(field("Location").value).toBe("Hall");
  });

  test("Use the file loads the file's values", async () => {
    const dialog = await conflict();
    await press(button("Use the file", dialog));
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(field("Name").value).toBe("Kitchen");
    expect((button("Save") as HTMLButtonElement).disabled).toBe(true);
  });

  test("Show the file's values loads them and marks your edits", async () => {
    const dialog = await conflict();
    await press(button("Show the file's values", dialog));
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(field("Name").value).toBe("Kitchen");
    expect(document.body.textContent).toContain("Name: Office");
    expect(field("Name").closest(".settings-highlight")).not.toBeNull();
    expect(field("Location").closest(".settings-highlight")).toBeNull();
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
    await type("Name", "Office");
    writes.push(() => json(invalid("e2"), 412));
    await press(button("Save"));
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
    expect(field("Name").value).toBe("Office");
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
    await type("Name", "Office");
    const gets = writesOf("GET").length;
    FakeEventSource.last.emit("config", { etag: "e9" });
    expect(await screen.findByText(/The file changed on disk/)).toBeTruthy();
    expect(field("Name").value).toBe("Office");
    expect(writesOf("GET").length).toBe(gets);
  });

  test("a config event on a clean form reloads silently", async () => {
    await mount();
    await screen.findByRole("textbox", { name: "Name" });
    getConfig = () => json(applied("e9", config({ name: "Kitchen" })));
    FakeEventSource.last.emit("config", { etag: "e9" });
    await waitFor(() => expect(field("Name").value).toBe("Kitchen"));
    expect(screen.queryByText(/The file changed on disk/)).toBeNull();
  });

  test("the banner does not come back after the conflict is resolved", async () => {
    await mount();
    await type("Name", "Office");
    FakeEventSource.last.emit("config", { etag: "e9" });
    expect(await screen.findByText(/The file changed on disk/)).toBeTruthy();
    writes.push(() => json(applied("e9", config({ location: "Hall" })), 412));
    await press(button("Save"));
    const dialog = await screen.findByRole("dialog", { name: "Someone else changed the settings" });
    writes.push(() => json(applied("e10", config({ name: "Office", location: "Hall" }))));
    await press(button("Keep mine", dialog));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    await type("Name", "Office 2");
    expect(screen.queryByText(/The file changed on disk/)).toBeNull();
  });

  test("a reload that lands during a new edit keeps the edit and the old ETag", async () => {
    await mount();
    await screen.findByRole("textbox", { name: "Name" });
    let resolve: (r: Response) => void = () => {};
    getConfig = () =>
      new Promise<Response>((r) => {
        resolve = r;
      }) as unknown as Response;
    FakeEventSource.last.emit("config", { etag: "e9" });
    await type("Name", "Office");
    await act(async () => {
      resolve(json(applied("e9", config({ location: "Hall" }))));
    });
    expect(await screen.findByText(/The file changed on disk/)).toBeTruthy();
    expect(field("Name").value).toBe("Office");
    expect(field("Location").value).toBe("");
    writes.push(() => json(applied("e9", config({ location: "Hall" })), 412));
    await press(button("Save"));
    expect(writesOf("PATCH")[0]?.headers["If-Match"]).toBe('"e1"');
  });

  test("the echo of our own save is ignored", async () => {
    await mount();
    await screen.findByRole("textbox", { name: "Name" });
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
    await waitFor(() => expect(field("Name").value).toBe("Old"));
  });
  test("a 412 on restore shows the file on disk now", async () => {
    getConfig = () => json(invalid("e5", config({ name: "Old" })));
    await mount();
    await press(await screen.findByRole("button", { name: "Restore last working settings" }));
    const confirm = screen.getByRole("alertdialog", { name: "Restore the last working settings?" });
    writes.push(() => json(applied("e7", config({ name: "Fixed by hand" })), 412));
    await press(button("Restore", confirm));
    await waitFor(() => expect(field("Name").value).toBe("Fixed by hand"));
    expect(screen.queryByRole("dialog")).toBeNull();
  });
});

describe("licences", () => {
  test("shows the blob state and revoking sends the new list", async () => {
    await mount();
    FakeEventSource.last.emit(
      "status",
      status([{ ...HPLIP, state: "failed", message: "HP plug-in has no arm64 build" }]),
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
    await press(button("Save"));
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
    await press(button("Save"));
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
    await press(button("Save"));
    expect(writesOf("PATCH")[0]?.body).toEqual({
      licenses: { acceptedEulas: [{ ...HPLIP, textSha256: eulaSha }] },
    });
  });
});

test("a missing config points to setup", async () => {
  getConfig = () => problem(404, "config-missing");
  await mount();
  expect(await screen.findByText("printscan is not set up yet")).toBeTruthy();
  expect(screen.getByRole("link", { name: "Go to setup" }).getAttribute("href")).toBe("/setup");
});
