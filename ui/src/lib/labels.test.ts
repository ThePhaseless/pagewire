import { describe, expect, test } from "bun:test";
import type { Warning } from "../gen/api";
import { humanize, mediaLabel, optionLabel, optionTitle, warningText } from "./labels";
import { pwgMedia } from "./pwg-media.fixture";

const PWG = /^([a-z0-9]+)_([a-z0-9.-]+)_([0-9.]+)x([0-9.]+)(mm|in)$/;

describe("mediaLabel", () => {
  test("names and sizes", () => {
    expect(mediaLabel("iso_a4_210x297mm")).toBe("A4 (210 × 297 mm)");
    expect(mediaLabel("na_letter_8.5x11in")).toBe("Letter (8.5 × 11 in)");
    expect(mediaLabel("custom")).toBe("Custom");
  });

  test("spot checks", () => {
    expect(mediaLabel("na_number-10_4.125x9.5in")).toBe("Envelope #10 (4.125 × 9.5 in)");
    expect(mediaLabel("jis_b5_182x257mm")).toBe("JIS B5 (182 × 257 mm)");
    expect(mediaLabel("na_5x7_5x7in")).toBe("5 × 7 in");
    expect(mediaLabel("custom_min_3x5in")).toBe("Smallest custom size (3 × 5 in)");
    expect(mediaLabel("om_small-photo_100x150mm")).toBe("Small Photo (100 × 150 mm)");
    expect(mediaLabel("iso_dl_110x220mm")).toBe("Envelope DL (110 × 220 mm)");
    expect(mediaLabel("na_index-5x8_5x8in")).toBe("Index card 5 × 8 (5 × 8 in)");
    expect(mediaLabel("jpn_hagaki_100x148mm")).toBe("Postcard (Hagaki) (100 × 148 mm)");
    expect(mediaLabel("oe_photo-l_3.5x5in")).toBe("Photo L (3.5 × 5 in)");
    expect(mediaLabel("na_invoice_5.5x8.5in")).toBe("Statement (5.5 × 8.5 in)");
    expect(mediaLabel("custom_max_8.5x14in")).toBe("Largest custom size (8.5 × 14 in)");
    // Generic rule: short letter+digit tokens upper-cased, words capitalised, class prefixes.
    expect(mediaLabel("iso_a4-extra_235.5x322.3mm")).toBe("A4 Extra (235.5 × 322.3 mm)");
    expect(mediaLabel("iso_sra3_320x450mm")).toBe("SRA3 (320 × 450 mm)");
    expect(mediaLabel("iso_a1x3_841x1783mm")).toBe("A1X3 (841 × 1783 mm)");
    expect(mediaLabel("prc_16k_146x215mm")).toBe("PRC 16K (146 × 215 mm)");
    expect(mediaLabel("roc_8k_10.75x15.5in")).toBe("ROC 8K (10.75 × 15.5 in)");
    expect(mediaLabel("custom_foo-bar_100x200mm")).toBe("Custom Foo Bar (100 × 200 mm)");
    // A size token inside a name reads as a size, not "14X18".
    expect(mediaLabel("oe_photo-14x18_14x18in")).toBe("Photo 14 × 18 (14 × 18 in)");
  });

  test("every PWG 5101.1 keyword reads as a name and a size", () => {
    expect(pwgMedia.length).toBe(210);
    for (const kw of pwgMedia) {
      const [, , , w, h, unit] = PWG.exec(kw) ?? [];
      const label = mediaLabel(kw);
      expect(label).not.toBe(kw);
      expect(label).not.toContain("_");
      const size = `${w} × ${h} ${unit}`;
      expect(label.endsWith(`(${size})`) || label === size).toBe(true);
      if (label !== size) expect(label).not.toMatch(/^[a-z]/);
    }
  });
});

test("humanize", () => {
  expect(humanize("tray-1")).toBe("Tray 1");
  expect(humanize("by-pass-tray")).toBe("By pass tray");
  expect(humanize("stacker_2")).toBe("Stacker 2");
  expect(humanize("")).toBe("");
});

describe("optionLabel", () => {
  const known: [Parameters<typeof optionLabel>, string][] = [
    [["sides", "one-sided", "printer"], "One-sided"],
    [["sides", "two-sided-long-edge", "printer"], "Two-sided, long edge"],
    [["sides", "two-sided-short-edge", "printer"], "Two-sided, short edge"],
    [["colorMode", "auto", "printer"], "Automatic"],
    [["colorMode", "color", "printer"], "Colour"],
    [["colorMode", "monochrome", "printer"], "Black and white"],
    [["colorMode", "color", "scanner"], "Colour"],
    [["colorMode", "grayscale", "scanner"], "Greyscale"],
    [["colorMode", "blackandwhite", "scanner"], "Black and white"],
    [["quality", "draft", "printer"], "Draft"],
    [["quality", "normal", "printer"], "Normal"],
    [["quality", "high", "printer"], "High"],
    [["contentOptimize", "auto", "printer"], "Automatic"],
    [["contentOptimize", "graphic", "printer"], "Graphics"],
    [["contentOptimize", "photo", "printer"], "Photo"],
    [["contentOptimize", "text", "printer"], "Text"],
    [["contentOptimize", "text-and-graphic", "printer"], "Text and graphics"],
    [["orientation", "portrait", "printer"], "Portrait"],
    [["orientation", "landscape", "printer"], "Landscape"],
    [["orientation", "reverse-landscape", "printer"], "Landscape, upside down"],
    [["orientation", "reverse-portrait", "printer"], "Portrait, upside down"],
    [["printScaling", "auto", "printer"], "Automatic"],
    [["printScaling", "auto-fit", "printer"], "Shrink to fit"],
    [["printScaling", "fill", "printer"], "Fill page"],
    [["printScaling", "fit", "printer"], "Fit to page"],
    [["printScaling", "none", "printer"], "Actual size"],
    [["mediaSource", "auto", "printer"], "Automatic"],
    [["mediaSource", "main", "printer"], "Main tray"],
    [["mediaSource", "manual", "printer"], "Manual feed"],
    [["mediaSource", "by-pass-tray", "printer"], "Bypass tray"],
    [["mediaSource", "alternate", "printer"], "Alternate tray"],
    [["mediaSource", "envelope", "printer"], "Envelope feeder"],
    [["mediaSource", "large-capacity", "printer"], "High-capacity tray"],
    [["mediaSource", "photo", "printer"], "Photo tray"],
    [["mediaSource", "top", "printer"], "Top tray"],
    [["mediaSource", "middle", "printer"], "Middle tray"],
    [["mediaSource", "bottom", "printer"], "Bottom tray"],
    [["mediaSource", "tray-2", "printer"], "Tray 2"],
    [["mediaType", "stationery", "printer"], "Plain paper"],
    [["mediaType", "stationery-letterhead", "printer"], "Letterhead"],
    [["mediaType", "stationery-heavyweight", "printer"], "Heavy paper"],
    [["mediaType", "stationery-lightweight", "printer"], "Light paper"],
    [["mediaType", "photographic", "printer"], "Photo paper"],
    [["mediaType", "photographic-glossy", "printer"], "Glossy photo paper"],
    [["mediaType", "photographic-matte", "printer"], "Matte photo paper"],
    [["mediaType", "photographic-semi-gloss", "printer"], "Semi-gloss photo paper"],
    [["mediaType", "envelope", "printer"], "Envelope"],
    [["mediaType", "labels", "printer"], "Labels"],
    [["mediaType", "transparency", "printer"], "Transparency"],
    [["mediaType", "cardstock", "printer"], "Card"],
    [["outputBin", "auto", "printer"], "Automatic"],
    [["outputBin", "face-down", "printer"], "Face down"],
    [["outputBin", "face-up", "printer"], "Face up"],
    [["outputBin", "top", "printer"], "Top tray"],
    [["outputBin", "large-capacity", "printer"], "Large capacity tray"],
    [["outputBin", "stacker-1", "printer"], "Stacker 1"],
    [["outputBin", "mailbox-3", "printer"], "Mailbox 3"],
    [["finishings", "none", "printer"], "None"],
    [["finishings", "staple", "printer"], "Staple"],
    [["finishings", "punch", "printer"], "Hole punch"],
    [["finishings", "fold", "printer"], "Fold"],
    [["finishings", "bind", "printer"], "Bind"],
    [["finishings", "saddle-stitch", "printer"], "Saddle stitch"],
    [["finishings", "booklet-maker", "printer"], "Booklet"],
    [["finishings", "staple-top-left", "printer"], "Staple top left"],
    [["source", "platen", "scanner"], "Flatbed"],
    [["source", "adf", "scanner"], "Document feeder"],
    [["source", "adf-duplex", "scanner"], "Document feeder, both sides"],
    [["intent", "document", "scanner"], "Document"],
    [["intent", "text-and-graphic", "scanner"], "Text and graphics"],
    [["intent", "photo", "scanner"], "Photo"],
    [["intent", "preview", "scanner"], "Preview"],
    [["intent", "object", "scanner"], "Object"],
    [["intent", "business-card", "scanner"], "Business card"],
    [["format", "pdf", "scanner"], "PDF"],
    [["format", "jpeg", "scanner"], "JPEG"],
    [["media", "iso_a4_210x297mm", "printer"], "A4 (210 × 297 mm)"],
    [["resolution", 300, "printer"], "300 dpi"],
    [["resolution", "600", "scanner"], "600 dpi"],
    [["numberUp", 1, "printer"], "1 page per sheet"],
    [["numberUp", "4", "printer"], "4 pages per sheet"],
  ];
  for (const [args, want] of known) {
    test(`${args[0]} ${args[1]} (${args[2]})`, () => expect(optionLabel(...args)).toBe(want));
  }
});

test("optionTitle", () => {
  expect(optionTitle("media", "printer")).toBe("Paper size");
  expect(optionTitle("source", "scanner")).toBe("Scan from");
  expect(optionTitle("colorMode", "scanner")).toBe("Colour");
});

describe("warningText", () => {
  const w = (o: Partial<Warning>): Warning => ({
    option: "colorMode",
    value: "auto",
    using: "monochrome",
    message: "go text",
    ...o,
  });
  test("a colour drop names both values by label", () => {
    expect(warningText(w({}), "printer")).toBe(
      "The default Colour “Automatic” is not offered by this printer; it uses “Black and white”.",
    );
  });
  test("a drop without a device default", () => {
    expect(
      warningText(w({ option: "media", value: "na_letter_8.5x11in", using: "" }), "printer"),
    ).toBe("The default Paper size “Letter (8.5 × 11 in)” is not offered by this printer.");
    expect(warningText(w({ option: "colorMode", value: "grayscale", using: "" }), "scanner")).toBe(
      "The default Colour “Greyscale” is not offered by this scanner.",
    );
  });
  test("other returns the message", () => {
    expect(
      warningText(
        w({ option: "other", message: "Could not set the printer's defaults: x" }),
        "printer",
      ),
    ).toBe("Could not set the printer's defaults: x");
  });
});
