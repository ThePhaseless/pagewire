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
  name: "Home",
  printers: [
    {
      id: "hp",
      mode: "usb",
      usb: { vendorId: "03f0", productId: "2b17", serial: "" },
      defaults: { sides: "one-sided" } as Record<string, string>,
    },
    { id: "samsung", mode: "usb", defaults: {} as Record<string, string> },
  ],
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

  test("removing a printer's USB selector sends the whole printers list", () => {
    const edited = structuredClone(base);
    delete (edited.printers[0] as { usb?: unknown }).usb;
    const patch = diff(base, edited);
    expect(patch).toEqual({ printers: edited.printers });
    expect(patch.printers).toHaveLength(2);
  });

  test("a default removed from one entry sends the list with the other entry unchanged", () => {
    const edited = structuredClone(base);
    delete edited.printers[0]?.defaults.sides;
    const patch = diff(base, edited) as { printers: typeof base.printers };
    expect(patch.printers[0]?.defaults).toEqual({});
    expect(patch.printers[1]).toEqual(base.printers[1]);
  });

  test("a removed key, or one set to undefined, becomes null", () => {
    const edited = structuredClone(base) as Record<string, unknown> & typeof base;
    delete (edited.print as { maxUploadMiB?: number }).maxUploadMiB;
    expect(diff(base, edited)).toEqual({ print: { maxUploadMiB: null } });
    expect(JSON.stringify(diff(base, edited))).toBe('{"print":{"maxUploadMiB":null}}');
    expect(diff(base, { ...base, print: undefined })).toEqual({ print: null });
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
    const from = { scan: { retentionDays: 7 } };
    const extra = { a: 1, b: "two" };
    expect(diff(from, { scan: { retentionDays: 7, extra } })).toEqual({ scan: { extra } });
  });

  test("applying the patch to the base gives the edited config", () => {
    const edited = structuredClone(base) as typeof base & { scan?: { retentionDays: number } };
    edited.name = "Office";
    edited.scan = { retentionDays: 3 };
    const usb = edited.printers[0]?.usb;
    if (usb) usb.serial = "X1";
    edited.licenses.acceptedEulas = [];
    expect(apply(base, diff(base, edited))).toEqual(edited);
  });
});

describe("changedPaths and valueAt", () => {
  test("lists leaf pointers, with arrays and null as leaves", () => {
    const patch = {
      name: "x",
      print: { maxUploadMiB: null, extra: 2 },
      printers: [{ id: "hp" }],
      licenses: { acceptedEulas: [{ id: "a", version: "1" }] },
    };
    expect(changedPaths(patch)).toEqual([
      "/name",
      "/print/maxUploadMiB",
      "/print/extra",
      "/printers",
      "/licenses/acceptedEulas",
    ]);
  });

  test("an empty patch has no paths", () => {
    expect(changedPaths({})).toEqual([]);
  });

  test("valueAt follows a pointer and gives undefined for a missing key", () => {
    expect(valueAt(base, "/print/maxUploadMiB")).toBe(50);
    expect(valueAt(base, "/licenses/acceptedEulas")).toEqual([{ id: "a", version: "1" }]);
    expect(valueAt(base, "/print/nope")).toBeUndefined();
    expect(valueAt(null, "/name")).toBeUndefined();
  });
});
