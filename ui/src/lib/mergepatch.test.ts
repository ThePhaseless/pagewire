import { describe, expect, test } from "bun:test";
import { changedPaths, diff, valueAt } from "./mergepatch";

// RFC 7396 apply, used only to check that diff() round-trips.
function apply(target: unknown, patch: unknown): unknown {
  if (typeof patch !== "object" || patch === null || Array.isArray(patch)) return patch;
  const out: Record<string, unknown> =
    typeof target === "object" && target !== null && !Array.isArray(target)
      ? { ...(target as Record<string, unknown>) }
      : {};
  for (const [k, v] of Object.entries(patch)) {
    if (v === null) delete out[k];
    else out[k] = apply(out[k], v);
  }
  return out;
}

const base = {
  name: "Printer",
  device: { mode: "usb", usb: { vendorId: "03f0", productId: "2b17", serial: "" } },
  print: { maxUploadMiB: 50 },
  licenses: { acceptedEulas: [{ id: "a", version: "1" }] },
};

describe("diff", () => {
  test("equal configs give an empty patch", () => {
    expect(diff(base, structuredClone(base))).toEqual({});
  });

  test("a changed scalar appears at its path only", () => {
    expect(diff(base, { ...base, print: { maxUploadMiB: 10 } })).toEqual({
      print: { maxUploadMiB: 10 },
    });
  });

  test("removing the USB selector sends null", () => {
    const edited = structuredClone(base) as Record<string, unknown> & typeof base;
    delete (edited.device as { usb?: unknown }).usb;
    const patch = diff(base, edited);
    expect(patch).toEqual({ device: { usb: null } });
    expect(JSON.stringify(patch)).toBe('{"device":{"usb":null}}');
  });

  test("a key set to undefined also becomes null", () => {
    expect(diff(base, { ...base, device: { mode: "usb", usb: undefined } })).toEqual({
      device: { usb: null },
    });
  });

  test("arrays are replaced whole", () => {
    const acceptedEulas = [
      { id: "a", version: "1" },
      { id: "b", version: "2" },
    ];
    expect(diff(base, { ...base, licenses: { acceptedEulas } })).toEqual({
      licenses: { acceptedEulas },
    });
    expect(diff(base, { ...base, licenses: { acceptedEulas: [] } })).toEqual({
      licenses: { acceptedEulas: [] },
    });
  });

  test("an added object is sent whole", () => {
    const from = { device: { mode: "usb" } };
    const usb = { vendorId: "04b8", productId: "0001", serial: "" };
    expect(diff(from, { device: { mode: "usb", usb } })).toEqual({ device: { usb } });
  });

  test("applying the patch to the base gives the edited config", () => {
    const edited = structuredClone(base) as typeof base & { location?: string };
    edited.name = "Office";
    edited.location = "Hall";
    edited.device.usb.serial = "X1";
    edited.licenses.acceptedEulas = [];
    expect(apply(base, diff(base, edited))).toEqual(edited);
  });
});

describe("changedPaths and valueAt", () => {
  test("lists leaf pointers, with arrays and null as leaves", () => {
    const patch = {
      name: "x",
      device: { usb: null, driver: "ps" },
      licenses: { acceptedEulas: [{ id: "a", version: "1" }] },
    };
    expect(changedPaths(patch)).toEqual([
      "/name",
      "/device/usb",
      "/device/driver",
      "/licenses/acceptedEulas",
    ]);
  });

  test("an empty patch has no paths", () => {
    expect(changedPaths({})).toEqual([]);
  });

  test("valueAt follows a pointer and gives undefined for a missing key", () => {
    expect(valueAt(base, "/device/usb/vendorId")).toBe("03f0");
    expect(valueAt(base, "/licenses/acceptedEulas")).toEqual([{ id: "a", version: "1" }]);
    expect(valueAt(base, "/device/usb/nope")).toBeUndefined();
    expect(valueAt(null, "/name")).toBeUndefined();
  });
});
