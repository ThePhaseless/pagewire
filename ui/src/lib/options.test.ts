import { describe, expect, test } from "bun:test";
import type { PrintDefaults, PrinterCaps, ScanDefaults, ScannerCaps } from "../gen/api";
import {
  copiesLimit,
  optionKinds,
  optionState,
  preselect,
  printerGroups,
  printKinds,
  pruneUnsupported,
  scanKinds,
  setDefault,
  toRequest,
} from "./options";

const printerCaps = (c: Partial<PrinterCaps> = {}): PrinterCaps => ({
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
  ...c,
});

const scannerCaps = (c: Partial<ScannerCaps> = {}): ScannerCaps => ({
  sources: [],
  resolutions: [],
  colorModes: [],
  adf: false,
  duplex: false,
  intents: [],
  ...c,
});

test("the kinds match device.OptionKinds", () => {
  expect(optionKinds).toEqual([
    "media",
    "mediaSource",
    "mediaType",
    "sides",
    "colorMode",
    "quality",
    "resolution",
    "contentOptimize",
    "orientation",
    "numberUp",
    "printScaling",
    "outputBin",
    "finishings",
    "copies",
    "source",
    "intent",
    "format",
    "other",
  ]);
  expect(printerGroups.flatMap((g) => g.kinds)).toEqual(printKinds);
  expect(printerGroups.map((g) => g.title)).toEqual(["Paper", "Printing", "Layout", "Output"]);
  expect(scanKinds).toEqual(["source", "resolution", "colorMode", "intent", "format"]);
});

describe("the three reasons", () => {
  test("not connected", () => {
    expect(optionState("sides", undefined, "printer").reason).toBe(
      "pagewire shows this printer's options once it is connected.",
    );
    expect(optionState("source", undefined, "scanner").reason).toBe(
      "pagewire shows this scanner's options once it is connected.",
    );
    expect(optionState("format", undefined, "scanner").reason).not.toBeNull();
  });

  test("not reported", () => {
    expect(optionState("finishings", printerCaps(), "printer").reason).toBe(
      "Not reported by this printer.",
    );
    expect(optionState("intent", scannerCaps(), "scanner").reason).toBe(
      "Not reported by this scanner.",
    );
    // A closed kind whose list holds only values outside its enum is not reported either.
    expect(
      optionState("colorMode", printerCaps({ colorModes: ["process-monochrome"] }), "printer")
        .reason,
    ).toBe("Not reported by this printer.");
  });

  test("one value", () => {
    const one = (kind: Parameters<typeof optionState>[0], c: Partial<PrinterCaps>) =>
      optionState(kind, printerCaps(c), "printer").reason;
    expect(one("sides", { sides: ["one-sided"] })).toBe("This printer only prints one-sided.");
    expect(one("colorMode", { colorModes: ["monochrome"] })).toBe(
      "This printer only prints in black and white.",
    );
    expect(one("media", { media: ["iso_a4_210x297mm"] })).toBe(
      "This printer only offers A4 (210 × 297 mm).",
    );
    expect(one("sides", { sides: ["two-sided-long-edge"] })).toBe(
      "This printer only offers Two-sided, long edge.",
    );
    const scan = (c: Partial<ScannerCaps>, kind: Parameters<typeof optionState>[0] = "source") =>
      optionState(kind, scannerCaps(c), "scanner").reason;
    expect(scan({ sources: ["platen"] })).toBe("This scanner has no document feeder.");
    expect(scan({ sources: ["adf"] })).toBe("This scanner has no flatbed.");
    expect(scan({ colorModes: ["grayscale"] }, "colorMode")).toBe(
      "This scanner only scans in greyscale.",
    );
    expect(scan({ intents: ["photo"] }, "intent")).toBe("This scanner only offers Photo.");
  });

  test("two or more values have no reason", () => {
    expect(
      optionState("sides", printerCaps({ sides: ["one-sided", "two-sided-long-edge"] }), "printer")
        .reason,
    ).toBeNull();
    expect(optionState("format", scannerCaps(), "scanner")).toEqual({
      values: ["pdf", "jpeg"],
      deviceDefault: "pdf",
      reason: null,
      known: true,
    });
  });
});

test("a closed kind drops an unknown device value", () => {
  const s = optionState(
    "colorMode",
    printerCaps({
      colorModes: ["auto", "auto-monochrome", "monochrome"],
      colorModeDefault: "auto-monochrome",
    }),
    "printer",
  );
  expect(s.values).toEqual(["auto", "monochrome"]);
  // The device default is not in the list, so the first value stands in.
  expect(s.deviceDefault).toBe("auto");
});

test("open kinds keep device values in device order", () => {
  const s = optionState(
    "mediaSource",
    printerCaps({
      mediaSources: ["tray-2", "auto", "tray-1", "auto"],
      mediaSourceDefault: "tray-1",
    }),
    "printer",
  );
  expect(s.values).toEqual(["tray-2", "auto", "tray-1"]);
  expect(s.deviceDefault).toBe("tray-1");
});

test("resolution: scan clamped to 75..1200, printer to the config's 75..2400, both ascending", () => {
  const scan = optionState(
    "resolution",
    scannerCaps({ resolutions: [2400, 300, 50, 1200, 75, 600] }),
    "scanner",
  );
  expect(scan.values).toEqual([75, 300, 600, 1200]);
  expect(scan.deviceDefault).toBe(75);
  const print = optionState(
    "resolution",
    printerCaps({ resolutions: [2400, 600, 1200, 4800], resolutionDefault: 600 }),
    "printer",
  );
  expect(print.values).toEqual([600, 1200, 2400]);
  expect(print.deviceDefault).toBe(600);
});

test("the device default is not in the list", () => {
  const s = optionState(
    "numberUp",
    printerCaps({ numberUp: [4, 1, 2], numberUpDefault: 0 }),
    "printer",
  );
  expect(s.values).toEqual([1, 2, 4]);
  expect(s.deviceDefault).toBe(1);
  expect(optionState("finishings", printerCaps(), "printer").deviceDefault).toBeUndefined();
});

test("preselect: the configured value when offered, else the device default", () => {
  const s = optionState(
    "sides",
    printerCaps({ sides: ["one-sided", "two-sided-long-edge"], sidesDefault: "one-sided" }),
    "printer",
  );
  expect(preselect("sides", s, "two-sided-long-edge")).toBe("two-sided-long-edge");
  expect(preselect("sides", s, "two-sided-short-edge")).toBe("one-sided");
  expect(preselect("sides", s, undefined)).toBe("one-sided");
});

describe("toRequest", () => {
  test("print leaves hidden kinds and hidden copies out", () => {
    const caps = printerCaps({
      sides: ["one-sided", "two-sided-long-edge"],
      colorModes: ["monochrome"],
      media: ["iso_a4_210x297mm", "na_letter_8.5x11in"],
      mediaDefault: "iso_a4_210x297mm",
      copiesMax: 1,
    });
    expect(
      toRequest("printer", caps, {
        copies: 3,
        sides: "two-sided-long-edge",
        colorMode: "monochrome",
        quality: "high",
      }),
    ).toEqual({ sides: "two-sided-long-edge", media: "iso_a4_210x297mm" });
  });

  test("print sends copies above 1 when the field is shown, clamped to copiesMax", () => {
    const caps = printerCaps({ copiesMax: 5 });
    expect(toRequest("printer", caps, { copies: 9 })).toEqual({ copies: 5 });
    expect(toRequest("printer", caps, { copies: 1 })).toEqual({});
    expect(toRequest("printer", undefined, { copies: 2, sides: "one-sided" })).toEqual({});
    expect(copiesLimit(printerCaps({ copiesMax: 999 }))).toBe(99);
    expect(copiesLimit(printerCaps({ copiesMax: 1 }))).toBe(0);
    expect(copiesLimit(undefined)).toBe(0);
  });

  test("scan sends a hidden kind's single value and leaves a hidden intent out", () => {
    const caps = scannerCaps({
      sources: ["platen"],
      resolutions: [300],
      colorModes: ["color", "grayscale"],
      intents: ["photo"],
    });
    expect(
      toRequest("scanner", caps, { colorMode: "grayscale", format: "jpeg", intent: "photo" }),
    ).toEqual({ source: "platen", resolution: 300, colorMode: "grayscale", format: "jpeg" });
    expect(
      toRequest("scanner", scannerCaps({ sources: ["platen"], intents: ["document", "photo"] }), {
        intent: "photo",
      }),
    ).toEqual({ source: "platen", intent: "photo", format: "pdf" });
  });
});

describe("defaults", () => {
  test("setDefault with the default entry removes the key", () => {
    const d: PrintDefaults = { sides: "two-sided-long-edge", resolution: 600 };
    expect(setDefault(d, "sides", "")).toEqual({ resolution: 600 });
    expect(setDefault(d, "resolution", 0)).toEqual({ sides: "two-sided-long-edge" });
    expect(setDefault(d, "sides", "one-sided")).toEqual({ sides: "one-sided", resolution: 600 });
    expect(d).toEqual({ sides: "two-sided-long-edge", resolution: 600 });
  });

  test("pruneUnsupported drops what the known caps do not offer, and nothing without caps", () => {
    const d: PrintDefaults = {
      sides: "two-sided-long-edge",
      colorMode: "color",
      media: "iso_a4_210x297mm",
    };
    const caps = printerCaps({ sides: ["one-sided"], colorModes: ["color", "monochrome"] });
    expect(pruneUnsupported(d, caps, "printer")).toEqual({ colorMode: "color" });
    expect(pruneUnsupported(d, undefined, "printer")).toEqual(d);
    expect(
      pruneUnsupported(
        { format: "jpeg", source: "adf" } as ScanDefaults,
        scannerCaps({ sources: ["platen"] }),
        "scanner",
      ),
    ).toEqual({ format: "jpeg" });
  });
});
