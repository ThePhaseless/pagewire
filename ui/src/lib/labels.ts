// Human-readable labels for every option value (A22). Never returns a raw PWG or IPP keyword.
import type { Warning } from "../gen/api";
import { labelStrings as t } from "./labels.strings";

export type DeviceRole = "printer" | "scanner";
export type OptionKind = Warning["option"];

const PWG_MEDIA = /^([a-z0-9]+)_([a-z0-9.-]+)_([0-9.]+)x([0-9.]+)(mm|in)$/;
const SIZE_TOKEN = /^([0-9.]+)x([0-9.]+)$/;
const CODE_TOKEN = /^[a-z]{0,3}[0-9]+[a-z0-9]*$/;

const capitalise = (s: string) => s.charAt(0).toUpperCase() + s.slice(1);

/** "tray-1" → "Tray 1": the generic fallback for any keyword. */
export function humanize(keyword: string): string {
  return capitalise(keyword.replace(/[-_]/g, " "));
}

function mediaName(cls: string, name: string): string {
  const override = t.mediaNames[`${cls}_${name}`];
  if (override) return override;
  if (SIZE_TOKEN.test(name)) return "";
  const words = name.split("-").map((tok) => {
    const size = SIZE_TOKEN.exec(tok);
    if (size) return t.sizeToken(size[1] ?? "", size[2] ?? "");
    return CODE_TOKEN.test(tok) ? tok.toUpperCase() : capitalise(tok);
  });
  return (t.mediaClass[cls] ?? "") + words.join(" ");
}

/** "iso_a4_210x297mm" → "A4 (210 × 297 mm)"; a keyword that does not parse goes through humanize. */
export function mediaLabel(keyword: string): string {
  const m = PWG_MEDIA.exec(keyword);
  if (!m) return humanize(keyword);
  const [, cls = "", name = "", w = "", h = "", unit = ""] = m;
  return t.mediaSize(mediaName(cls, name), w, h, unit);
}

/** The label of one value of `kind`. Numbers may come as decimal strings (Warning.value). */
export function optionLabel(kind: OptionKind, value: string | number, device: DeviceRole): string {
  const v = String(value);
  const map =
    kind === "colorMode"
      ? device === "printer"
        ? t.printColorMode
        : t.scanColorMode
      : t.values[kind];
  const known = map?.[v];
  if (known) return known;
  switch (kind) {
    case "media":
      return mediaLabel(v);
    case "resolution":
      return t.dpi(Number(v));
    case "numberUp":
      return t.pagesPerSheet(Number(v));
    case "outputBin":
      return humanize(v) + (t.binPositions.includes(v) ? t.binSuffix : "");
    default:
      return humanize(v);
  }
}

/** The field label of `kind`. */
export function optionTitle(kind: Exclude<OptionKind, "other">, _device: DeviceRole): string {
  return t.titles[kind];
}

/** The sentence for one structured warning; `other` warnings carry their own message. */
export function warningText(w: Warning, device: DeviceRole): string {
  if (w.option === "other") return w.message;
  return t.warning(
    optionTitle(w.option, device),
    optionLabel(w.option, w.value, device),
    device,
    w.using && optionLabel(w.option, w.using, device),
  );
}
