import { describe, expect, test } from "bun:test";
import type { USBDevice } from "./gen/api";
import { isNeverPrinterOrScanner } from "./usb";

const dev = (...classes: number[]): USBDevice => ({
  vendorId: "0000",
  productId: "0000",
  serial: "",
  manufacturer: "",
  product: "",
  bus: 1,
  address: 1,
  interfaces: classes.map((c) => ({ class: c, subClass: 0, protocol: 0 })),
  deviceId: "",
});

describe("isNeverPrinterOrScanner", () => {
  const cases: [string, USBDevice, boolean][] = [
    ["root hub (only 0x09)", dev(0x09), true],
    ["QEMU tablet (only 0x03)", dev(0x03), true],
    ["webcam (0x0e + 0x01)", dev(0x0e, 0x0e, 0x01, 0x01), true],
    ["Bluetooth dongle (0xe0)", dev(0xe0, 0xe0), true],
    ["SCX-4300 (0xff + 0x07)", dev(0xff, 0x07), false],
    ["HP MFP with printer and HID buttons (0x07 + 0x03)", dev(0x07, 0x03), false],
    ["SANE-only scanner (0xff)", dev(0xff), false],
    ["still-image scanner (0x06)", dev(0x06), false],
    ["mass storage (0x08)", dev(0x08), false],
    ["zero interfaces", dev(), false],
    ["hub mixed with vendor-specific (0x09 + 0xff)", dev(0x09, 0xff), false],
  ];
  for (const [name, d, hidden] of cases) {
    test(name, () => expect(isNeverPrinterOrScanner(d)).toBe(hidden));
  }
});
