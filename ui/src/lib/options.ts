// What each option kind offers on one device (A23): the per-kind table, the shared groups,
// the state OptionField renders, and the request and config helpers every surface uses.
import { optionFieldStrings as r } from "../components/OptionField.strings";
import {
  type PrintDefaults,
  PrintDefaultsSchema,
  type PrinterCaps,
  type PrintOptions,
  type ScanDefaults,
  ScanDefaultsSchema,
  type ScannerCaps,
  type ScanSettings,
  WarningSchema,
} from "../gen/api";
import { type DeviceRole, optionLabel } from "./labels";
import { labelStrings } from "./labels.strings";

export type { DeviceRole } from "./labels";

/** Every option kind, the same strings as Go's device.OptionKinds (Warning.option is built from it). */
export const optionKinds = WarningSchema.shape.option.options;

/** The config key and request field of each kind is the kind itself. */
export type PrintKind = keyof PrintDefaults;
export type ScanKind = keyof ScanDefaults;
export type FieldKind = PrintKind | ScanKind;
export type OptionValue = string | number;

export type OptionState = {
  /** Supported values in device order (numbers ascending), filtered to the kind's enum or range. */
  values: OptionValue[];
  /** The device's own default when it is in `values`, else the first value. */
  deviceDefault: OptionValue | undefined;
  /** Why the control cannot be changed, or null when it offers a choice. */
  reason: string | null;
  /** False while the device's capabilities are unknown. */
  known: boolean;
};

type Spec<C> = {
  list: (c: C) => readonly OptionValue[];
  def?: (c: C) => OptionValue;
  allowed?: readonly string[];
  range?: readonly [number, number];
};

type IntSchema = { unwrap(): { minValue: number | null; maxValue: number | null } };
const rangeOf = (s: IntSchema) => {
  const n = s.unwrap();
  return [n.minValue ?? 0, n.maxValue ?? Number.MAX_SAFE_INTEGER] as const;
};
const p = PrintDefaultsSchema.shape;
const s = ScanDefaultsSchema.shape;

const printerSpecs: Record<PrintKind, Spec<PrinterCaps>> = {
  media: { list: (c) => c.media, def: (c) => c.mediaDefault },
  mediaSource: { list: (c) => c.mediaSources, def: (c) => c.mediaSourceDefault },
  mediaType: { list: (c) => c.mediaTypes, def: (c) => c.mediaTypeDefault },
  sides: { list: (c) => c.sides, def: (c) => c.sidesDefault, allowed: p.sides.unwrap().options },
  colorMode: {
    list: (c) => c.colorModes,
    def: (c) => c.colorModeDefault,
    allowed: p.colorMode.unwrap().options,
  },
  quality: {
    list: (c) => c.qualities,
    def: (c) => c.qualityDefault,
    allowed: p.quality.unwrap().options,
  },
  resolution: {
    list: (c) => c.resolutions,
    def: (c) => c.resolutionDefault,
    range: rangeOf(p.resolution),
  },
  contentOptimize: {
    list: (c) => c.contentOptimize,
    def: (c) => c.contentOptimizeDefault,
    allowed: p.contentOptimize.unwrap().options,
  },
  orientation: {
    list: (c) => c.orientations,
    def: (c) => c.orientationDefault,
    allowed: p.orientation.unwrap().options,
  },
  numberUp: { list: (c) => c.numberUp, def: (c) => c.numberUpDefault, range: rangeOf(p.numberUp) },
  printScaling: {
    list: (c) => c.printScalings,
    def: (c) => c.printScalingDefault,
    allowed: p.printScaling.unwrap().options,
  },
  outputBin: { list: (c) => c.outputBins, def: (c) => c.outputBinDefault },
  finishings: { list: (c) => c.finishings, def: (c) => c.finishingsDefault },
};

// eSCL reports no defaults, so the first supported value is the scanner's default.
const scannerSpecs: Record<ScanKind, Spec<ScannerCaps>> = {
  source: { list: (c) => c.sources, allowed: s.source.unwrap().options },
  resolution: { list: (c) => c.resolutions, range: rangeOf(s.resolution) },
  colorMode: { list: (c) => c.colorModes, allowed: s.colorMode.unwrap().options },
  intent: { list: (c) => c.intents, allowed: s.intent.unwrap().options },
  // pagewire's own output, so always both once the scanner is known.
  format: { list: () => s.format.unwrap().options, allowed: s.format.unwrap().options },
};

export type OptionGroup<K> = { id: string; title: string; kinds: K[] };

const g = labelStrings.groups;
export const printerGroups: OptionGroup<PrintKind>[] = [
  { id: "paper", title: g.paper, kinds: ["media", "mediaSource", "mediaType"] },
  {
    id: "printing",
    title: g.printing,
    kinds: ["sides", "colorMode", "quality", "resolution", "contentOptimize"],
  },
  { id: "layout", title: g.layout, kinds: ["orientation", "numberUp", "printScaling"] },
  { id: "output", title: g.output, kinds: ["outputBin", "finishings"] },
];
export const scannerGroups: OptionGroup<ScanKind>[] = [
  { id: "scan", title: g.scan, kinds: ["source", "resolution", "colorMode", "intent", "format"] },
];
export const printKinds: PrintKind[] = printerGroups.flatMap((x) => x.kinds);
export const scanKinds: ScanKind[] = scannerGroups.flatMap((x) => x.kinds);

/** resolution and numberUp hold numbers; their "no value" is 0, every other kind's is "". */
export const isNumeric = (kind: FieldKind) => kind === "resolution" || kind === "numberUp";

function specFor(kind: FieldKind, device: DeviceRole): Spec<PrinterCaps & ScannerCaps> | undefined {
  return (device === "printer" ? printerSpecs : scannerSpecs)[kind as PrintKind & ScanKind];
}

function reasonFor(kind: FieldKind, values: OptionValue[], device: DeviceRole): string | null {
  if (values.length === 0) return r.notReported(device);
  if (values.length > 1) return null;
  const v = values[0] as OptionValue;
  if (kind === "sides" && v === "one-sided") return r.oneSided;
  if (kind === "colorMode")
    return r.oneColorMode(device, optionLabel(kind, v, device).toLowerCase());
  if (kind === "source") return v === "platen" ? r.noFeeder : r.noFlatbed;
  return r.onlyOffers(device, optionLabel(kind, v, device));
}

/** What `kind` offers on a device with `caps` (undefined while not connected or not loaded). */
export function optionState(
  kind: FieldKind,
  caps: PrinterCaps | ScannerCaps | undefined,
  device: DeviceRole,
): OptionState {
  if (!caps)
    return { values: [], deviceDefault: undefined, reason: r.notConnected(device), known: false };
  const spec = specFor(kind, device);
  const c = caps as PrinterCaps & ScannerCaps;
  let values = [...new Set(spec?.list(c) ?? [])];
  const { allowed, range } = spec ?? {};
  if (allowed) values = values.filter((v) => allowed.includes(String(v)));
  if (range) {
    values = values
      .filter((v) => typeof v === "number" && v >= range[0] && v <= range[1])
      .sort((a, b) => Number(a) - Number(b));
  }
  const def = spec?.def?.(c);
  const deviceDefault = def !== undefined && values.includes(def) ? def : values[0];
  return { values, deviceDefault, reason: reasonFor(kind, values, device), known: true };
}

/** The value a job-mode control starts with: the configured one when offered, else the device's. */
export function preselect(
  _kind: FieldKind,
  state: OptionState,
  configured: OptionValue | undefined,
): OptionValue | undefined {
  return configured !== undefined && state.values.includes(configured)
    ? configured
    : state.deviceDefault;
}

/** How many copies Print may ask for; 0 hides the copies field. */
export function copiesLimit(caps: PrinterCaps | undefined): number {
  return caps && caps.copiesMax > 1 ? Math.min(caps.copiesMax, 99) : 0;
}

/**
 * The job-mode request from the chosen values. Print sends only the kinds whose control is shown,
 * and copies only above 1 while its field is shown. Scan sends a hidden source, resolution or
 * colour mode's single value, and leaves a hidden intent out.
 */
export function toRequest(
  device: "printer",
  caps: PrinterCaps | undefined,
  chosen: PrintOptions,
): PrintOptions;
export function toRequest(
  device: "scanner",
  caps: ScannerCaps | undefined,
  chosen: Partial<ScanSettings>,
): Partial<ScanSettings>;
export function toRequest(
  device: DeviceRole,
  caps: PrinterCaps | ScannerCaps | undefined,
  chosen: Record<string, unknown>,
): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const kind of device === "printer" ? printKinds : scanKinds) {
    const state = optionState(kind, caps, device);
    const sendHidden = device === "scanner" && kind !== "intent" && state.values.length === 1;
    if (state.reason && !sendHidden) continue;
    const v = preselect(kind, state, chosen[kind] as OptionValue | undefined);
    if (v !== undefined) out[kind] = v;
  }
  if (device === "printer") {
    const limit = copiesLimit(caps as PrinterCaps | undefined);
    const copies = Number(chosen.copies ?? 0);
    if (limit > 0 && copies > 1) out.copies = Math.min(copies, limit);
  }
  return out;
}

/** `defaults` without the values the known caps do not offer; unchanged while caps are unknown. */
export function pruneUnsupported<D extends object>(
  defaults: D,
  caps: PrinterCaps | ScannerCaps | undefined,
  device: DeviceRole,
): D {
  if (!caps) return defaults;
  const out = { ...defaults } as Record<string, unknown>;
  for (const [kind, v] of Object.entries(out)) {
    if (!specFor(kind as FieldKind, device)) continue;
    if (!optionState(kind as FieldKind, caps, device).values.includes(v as OptionValue)) {
      delete out[kind];
    }
  }
  return out as D;
}

/** `defaults` with `kind` set; the default entry ("" or 0) deletes the key instead. */
export function setDefault<D extends object>(
  defaults: D,
  kind: FieldKind,
  value: OptionValue | undefined,
): D {
  const out = { ...defaults } as Record<string, unknown>;
  if (value === undefined || value === "" || value === 0) delete out[kind];
  else out[kind] = value;
  return out as D;
}
