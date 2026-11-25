// Copy for Settings and its ConflictDialog. Shared words (Save, Cancel, Loading) and the device
// copy (families) come from ../strings; option titles and values from ../lib/labels.
import { strings } from "../strings";

type Role = "printer" | "scanner";

export const settingsStrings = {
  description:
    "Change your printers, scanners and their defaults. Saving updates pagewire's settings file.",
  loadFailed: "Could not load the settings.",
  missing: {
    title: "pagewire is not set up yet",
    body: "Finish setup to create the settings file.",
    action: "Go to setup",
  },
  detect: "Detect and add devices",
  sections: {
    general: "General",
    printers: "Printers",
    scanners: "Scanners",
    ignored: "Ignored devices",
    licences: "Licences",
  },
  fields: {
    title: "Title",
    titleHint: "Shown at the top of every page.",
    maxUpload: "Upload limit (MB)",
    maxUploadHint: "The largest file the Print page accepts, 1 to 500 MB.",
    retention: "Keep scans for (days)",
    retentionHint: "0 keeps scans forever.",
    name: "Name",
    nameHint: (role: Role) =>
      role === "printer"
        ? "Computers and phones may show the printer's own name instead."
        : "Shown in pagewire. Computers may show the scanner's own name instead.",
    location: "Location",
    locationHint: "Optional, for example Living room.",
    enabled: "In use",
    mode: "Connection",
    modeOptions: (role: Role): Record<string, string> =>
      role === "printer"
        ? { usb: "USB printer", simulated: "Simulated printer" }
        : { usb: "USB scanner", simulated: "Simulated scanner" },
    usb: "USB device",
    usbHint: "The device this entry uses. Plug it in to see it in the list.",
    anyDevice: "Any device not used by another entry",
    notDetected: (label: string) => `${label} (not plugged in)`,
    driver: "Printer driver",
    driverName: "Driver name",
    driverNameHint:
      "Exact driver name from the printer application; leave empty to choose automatically.",
    backend: "Scanner driver",
    automatic: "Automatic",
    families: strings.device.families,
    defaults: "Defaults",
  },
  edit: "Edit",
  done: "Done",
  remove: {
    action: "Remove",
    title: (name: string) => `Remove ${name}?`,
    body: "pagewire stops serving it once you save. You can add the device again with Detect and add devices.",
    confirm: "Remove",
  },
  unnamed: "Unnamed",
  noPrinters: "No printers yet.",
  noScanners: "No scanners yet.",
  noIgnored: "Setup offers every device it detects.",
  ignoredHint: "Setup does not offer these devices.",
  ignoredRoles: (roles: readonly string[]) =>
    roles.includes("printer") && roles.includes("scanner")
      ? "As printer and scanner"
      : roles.includes("printer")
        ? "As printer"
        : "As scanner",
  offerAgain: "Offer again",
  /** Labels for JSON pointers, used for field errors and in the conflict dialog. */
  paths: {
    "/name": "Title",
    "/printers": "Printers",
    "/scanners": "Scanners",
    "/ignoredDevices": "Ignored devices",
    "/print/maxUploadMiB": "Upload limit",
    "/scan/retentionDays": "Keep scans for",
    "/licenses/acceptedEulas": "Accepted licences",
  } as Record<string, string>,
  /** Labels of the fields inside one printer or scanner, keyed by the pointer below the entry. */
  entryPaths: {
    name: "Name",
    location: "Location",
    enabled: "In use",
    mode: "Connection",
    usb: "USB device",
    "usb/vendorId": "USB vendor ID",
    "usb/productId": "USB product ID",
    "usb/serial": "USB serial number",
    driver: "Printer driver",
    driverName: "Driver name",
    backend: "Scanner driver",
  } as Record<string, string>,
  entryPath: (entry: string, field: string) => `${entry}, ${field}`,
  /** A disabled entry in the conflict dialog's lists. */
  off: (name: string) => `${name} (off)`,
  notSet: "Not set",
  none: "None",
  noLicences: "No device needs vendor files.",
  blob: {
    installed: "Installed",
    missing: "Not downloaded",
    failed: "Download failed",
    disabled: "Not available",
  } as Record<string, string>,
  blobState: (state: string, message: string) => (message ? `${state}: ${message}` : state),
  blobLabel: "Vendor files",
  reset: "Reset",
  unsaved: "Unsaved changes",
  leave: {
    title: "Leave without saving?",
    body: "If you leave this page, your unsaved changes are lost.",
    stay: "Keep editing",
    confirm: "Leave",
  },
  saved: "Settings saved",
  invalidValues: "The server rejected some settings:",
  errorAt: (where: string, message: string) => `${where}: ${message}`,
  reviewNote:
    "The form now shows the file's values. Fields you had changed are marked with your value.",
  fileChanged:
    "The file changed on disk. Saving shows you the differences; Reset loads the file's version.",
  invalid: {
    title: "The settings file has errors",
    withLastGood: "pagewire keeps using the last working settings until the file is fixed.",
    withoutLastGood: "pagewire has no earlier working settings to fall back on. Fix the file.",
    location: (line: number, column: number) => `Line ${line}, column ${column}`,
    download: "Download the file",
    restore: "Restore last working settings",
    restoreTitle: "Restore the last working settings?",
    restoreBody:
      "pagewire rewrites the file with the last settings that worked. Your edits to the file and its comments will be lost.",
    restoreConfirm: "Restore",
    restoreFailed: "Could not restore the settings",
  },
  conflict: {
    title: "Someone else changed the settings",
    body: "The file changed while you were editing. These are the fields you changed:",
    field: "Setting",
    yours: "Yours",
    inFile: "In the file now",
    keepMine: "Keep mine",
    useFile: "Use the file",
    review: "Show the file's values",
    alsoChanged: "Also changed in the file",
    invalidTitle: "The file on disk changed and is now invalid",
    invalidBody: "Someone edited the file while you were editing here, and it now has errors:",
    replace: "Replace the file with my settings",
    replaceWarning:
      "pagewire writes your settings as a new file. The comments and layout of the file on disk will be lost.",
    replaceConfirm: "Replace the file",
    back: "Back",
  },
};
