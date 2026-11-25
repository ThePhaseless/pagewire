// Which USB devices can be left out of device lists: the rule reads only the interface classes
// (bInterfaceClass) the probe reports, so a printer or scanner is never hidden.
import type { USBDevice } from "./gen/api";

/** Printer interface class (0x07). */
export const USB_CLASS_PRINTER = 0x07;

/** Interface classes no printer or scanner exposes on its own. */
const NEVER_PRINTER_OR_SCANNER = new Set([
  0x01, // Audio: sound cards, the audio half of webcams
  0x03, // HID: keyboards, mice, tablets; MFP buttons always sit beside a printer or vendor interface
  0x09, // Hub: root hubs of every host controller and external hubs
  0x0e, // Video: webcams
  0xe0, // Wireless controller: Bluetooth adapters
]);

/**
 * True when every interface of the device is a hub, HID, audio, video or Bluetooth interface.
 * Devices with no interfaces, or any other class (printer, still image, vendor-specific, mass
 * storage, ...), are kept.
 */
export function isNeverPrinterOrScanner(dev: USBDevice): boolean {
  return (
    dev.interfaces.length > 0 && dev.interfaces.every((i) => NEVER_PRINTER_OR_SCANNER.has(i.class))
  );
}

/** True when the device has a printer interface. */
export function hasPrinterInterface(dev: USBDevice): boolean {
  return dev.interfaces.some((i) => i.class === USB_CLASS_PRINTER);
}
