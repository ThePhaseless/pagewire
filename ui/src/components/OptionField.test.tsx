import { afterEach, describe, expect, mock, test } from "bun:test";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import type { PrinterCaps, ScannerCaps } from "../gen/api";
import { optionState } from "../lib/options";
import { pwgMedia } from "../lib/pwg-media.fixture";
import { OptionField, type OptionFieldProps } from "./OptionField";

afterEach(cleanup);

const printer = (c: Partial<PrinterCaps>) =>
  ({
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
    resolutions: [],
    numberUp: [],
    ...c,
  }) as PrinterCaps;
const scanner = (c: Partial<ScannerCaps>) =>
  ({ sources: [], resolutions: [], colorModes: [], intents: [], ...c }) as ScannerCaps;

const twoSides = printer({
  sides: ["one-sided", "two-sided-long-edge"],
  sidesDefault: "one-sided",
});

function field(p: Partial<OptionFieldProps> & Pick<OptionFieldProps, "kind" | "state">) {
  return render(
    <OptionField device="printer" mode="job" value={undefined} onChange={() => {}} {...p} />,
  );
}

/** The option texts and values, from the hidden native select that lists every option. */
const nativeOptions = () =>
  [...document.querySelectorAll("select option")]
    .map((o) => ({ text: o.textContent ?? "", value: (o as HTMLOptionElement).value }))
    .filter((o) => o.value !== "");
const nativeSelect = () => document.querySelector("select") as HTMLSelectElement;
const trigger = (name: RegExp) => screen.getByRole("button", { name });

describe("job mode", () => {
  test("renders nothing for one value, not reported and not connected", () => {
    for (const state of [
      optionState("sides", printer({ sides: ["one-sided"] }), "printer"),
      optionState("sides", printer({}), "printer"),
      optionState("sides", undefined, "printer"),
    ]) {
      const { container } = field({ kind: "sides", state });
      expect(container.innerHTML).toBe("");
      cleanup();
    }
  });

  test("a dropdown preselected with the configured value, else the device default", () => {
    field({ kind: "sides", state: optionState("sides", twoSides, "printer") });
    const button = trigger(/Sides/);
    expect(button.getAttribute("aria-haspopup")).toBe("listbox");
    expect(button.textContent).toContain("One-sided");
    cleanup();
    field({
      kind: "sides",
      value: "two-sided-long-edge",
      state: optionState("sides", twoSides, "printer"),
    });
    expect(trigger(/Sides/).textContent).toContain("Two-sided, long edge");
  });

  test("format carries the PDF and JPEG hint", () => {
    field({
      kind: "format",
      device: "scanner",
      state: optionState("format", scanner({}), "scanner"),
    });
    expect(screen.getByText(/PDF keeps all pages in one file/)).toBeTruthy();
  });

  test("no option shows a raw keyword", () => {
    const caps = printer({
      media: [...pwgMedia],
      mediaSources: ["tray-1", "by-pass-tray", "large-capacity", "auto"],
    });
    for (const kind of ["media", "mediaSource"] as const) {
      field({ kind, state: optionState(kind, caps, "printer") });
      const opts = nativeOptions();
      expect(opts.length).toBeGreaterThan(1);
      for (const o of opts) {
        expect(o.text).not.toMatch(/_[0-9.]+x[0-9.]+(mm|in)$/);
        if (/[-_]/.test(o.value)) expect(o.text).not.toBe(o.value);
      }
      cleanup();
    }
  });
});

describe("defaults mode", () => {
  test("an unchangeable kind is disabled, with the reason as its accessible description", async () => {
    field({
      kind: "sides",
      mode: "defaults",
      state: optionState("sides", printer({ sides: ["one-sided"] }), "printer"),
    });
    const button = trigger(/Sides/);
    expect(button.hasAttribute("disabled") || button.getAttribute("aria-disabled") === "true").toBe(
      true,
    );
    await waitFor(() => {
      const ids = (button.getAttribute("aria-describedby") ?? "").split(" ").filter(Boolean);
      const text = ids.map((id) => document.getElementById(id)?.textContent ?? "").join(" ");
      expect(text).toContain("This printer only prints one-sided.");
    });
  });

  test("offers the printer default first and maps it to an empty value", () => {
    const onChange = mock((_: string | number) => {});
    field({
      kind: "sides",
      mode: "defaults",
      value: "two-sided-long-edge",
      onChange,
      state: optionState("sides", twoSides, "printer"),
    });
    expect(nativeOptions().map((o) => o.text)).toEqual([
      "Printer default (One-sided)",
      "One-sided",
      "Two-sided, long edge",
    ]);
    expect(trigger(/Sides/).textContent).toContain("Two-sided, long edge");
    fireEvent.change(nativeSelect(), { target: { value: "default" } });
    expect(onChange).toHaveBeenLastCalledWith("");
  });

  test("a numeric kind's default entry maps to 0, and a value to its number", () => {
    const onChange = mock((_: string | number) => {});
    field({
      kind: "resolution",
      mode: "defaults",
      value: 600,
      onChange,
      state: optionState("resolution", printer({ resolutions: [300, 600] }), "printer"),
    });
    fireEvent.change(nativeSelect(), { target: { value: "default" } });
    expect(onChange).toHaveBeenLastCalledWith(0);
    fireEvent.change(nativeSelect(), { target: { value: "300" } });
    expect(onChange).toHaveBeenLastCalledWith(300);
  });

  test("a saved value the printer does not offer shows the default entry and says so", () => {
    field({
      kind: "mediaType",
      mode: "defaults",
      value: "stationery-letterhead",
      state: optionState(
        "mediaType",
        printer({ mediaTypes: ["stationery", "photographic"] }),
        "printer",
      ),
    });
    expect(trigger(/Paper type/).textContent).toContain("Printer default (Plain paper)");
    expect(
      screen.getByText("Your saved “Letterhead” is not offered by this printer."),
    ).toBeTruthy();
  });

  test("while not connected the saved value stays shown, disabled, with the reason", () => {
    field({
      kind: "source",
      device: "scanner",
      mode: "defaults",
      value: "adf",
      state: optionState("source", undefined, "scanner"),
    });
    expect(trigger(/Scan from/).textContent).toContain("Document feeder");
    expect(nativeOptions().map((o) => o.text)).toEqual(["Scanner default", "Document feeder"]);
    expect(
      screen.getByText("pagewire shows this scanner's options once it is connected."),
    ).toBeTruthy();
  });
});
