// Copy for the setup wizard. Shared words (Back, Next, Loading) and the device copy shared with
// Status and Settings come from ../strings (strings.device); option titles and labels from
// ../lib/labels.
import { strings } from "../strings";

const stepOf = (n: number, total: number) => `Step ${n} of ${total}`;
type Role = "printer" | "scanner";

export const setupStrings = {
  title: "Set up pagewire",
  addTitle: "Add devices",
  progressLabel: "Setup steps",
  stepOf,
  /** The document title for a step: "Step 2 of 5: Names and locations". */
  stepTitle: (n: number, total: number, label: string) => `${stepOf(n, total)}: ${label}`,
  steps: {
    devices: "Choose devices",
    names: "Names and locations",
    drivers: "Drivers and licences",
    review: "Review and save",
    defaults: "Defaults",
  },
  loadFailed: "Could not load the setup data.",
  configInvalid: "The settings file has an error. Fix it in Settings, then add devices again.",
  alreadyDone: {
    title: "pagewire is already set up",
    body: "Change names, drivers or defaults in Settings, or look for more devices.",
    action: "Go to Settings",
    add: "Detect and add devices",
  },
  detect: {
    description: "pagewire looks for printers and scanners on USB. Choose how to use each one.",
    searchingTitle: "Looking for devices",
    waiting: "Waiting for the device to report.",
    plugInHint: strings.plugInHint,
    devicesTitle: "USB devices",
    noUsable:
      "None of these devices is a printer or scanner pagewire can use. Check the cable and that the device is switched on.",
    unnamedDevice: "Unnamed USB device",
    /** Disclosure over the devices that cannot be a printer or scanner (hubs, keyboards, ...). */
    otherDevices: (n: number) =>
      `Show ${n} other USB ${n === 1 ? "device" : "devices"} (hubs, keyboards, cameras…)`,
    usbIds: (vendorId: string, productId: string) => `USB ${vendorId}:${productId}`,
    serial: (serial: string) => `serial ${serial}`,
    simulatedName: (name: string) => `${name} (simulated)`,
    cannotUse: "pagewire cannot use this device as a printer or scanner.",
    /** The choice for one role of a device. */
    roleLabel: { printer: "As a printer", scanner: "As a scanner" } satisfies Record<Role, string>,
    use: { printer: "Use as printer", scanner: "Use as scanner" } satisfies Record<Role, string>,
    dontUse: "Don't use",
    inUse: (role: Role, name: string) =>
      role === "printer" ? `In use as printer ${name}` : `In use as scanner ${name}`,
    /** Disclosure over the devices the user chose not to use before. */
    notOffered: (n: number) => `Not offered (${n})`,
    notOfferedHint: "You chose not to use these. pagewire does not offer them again.",
    ignoredRoles: (roles: Role[]) =>
      `Not used as ${roles.map((r) => (r === "printer" ? "printer" : "scanner")).join(" or ")}`,
    offerAgain: "Offer again",
    detectAgain: "Detect again",
    detecting: "Looking again. The list updates by itself.",
    noUsbTitle: "No USB devices available",
    k8sIntro: "This server cannot reach USB devices yet. On Kubernetes pagewire needs:",
    k8sSteps: [
      "generic-device-plugin running on the node the devices are plugged into, sharing them as a resource such as devic.es/printer;",
      "PAGEWIRE_USB_RESOURCE set to that resource name on the pagewire Deployment;",
      "the printing namespace allowed to run privileged pods (host networking).",
    ],
    simBackend:
      "This server runs without Kubernetes and has no USB device attached, so it cannot reach a real printer or scanner.",
    simulatedOffer: "You can finish setup with a simulated printer and scanner now.",
    useSimulated: "Use a simulated printer",
    simulatedTitle: "Simulated printer and scanner",
    simulatedBody:
      "pagewire will run a test printer and a test scanner. Nothing goes to real hardware.",
    useUsb: "Use the USB devices instead",
  },
  names: {
    description: "How each device appears in pagewire and to computers on your network.",
    titleLabel: "Installation name",
    titleHint: "Shown at the top of every page.",
    nameLabel: "Name",
    printerHint: "Computers and phones may show the printer's own name instead.",
    scannerHint: "Shown in pagewire.",
    nameInvalid: "Enter 1 to 63 characters, without line breaks or tabs, not starting with a dash.",
    nameTaken: "Another printer already has this name.",
    locationLabel: "Location",
    locationHint: "Optional, for example Living room. Computers show it when you add the device.",
    locationInvalid: "Use at most 127 characters, without line breaks or tabs.",
    printerTitle: (name: string) => `Printer: ${name}`,
    scannerTitle: (name: string) => `Scanner: ${name}`,
    nothingChosen: "No device chosen. Saving keeps pagewire's settings without new devices.",
  },
  drivers: {
    description: "pagewire picked a driver from each device's USB details. You can override it.",
    explained: (family: string) => `pagewire picked the printer driver ${family}.`,
    scannerExplained: (family: string) => `pagewire picked the scanner driver ${family}.`,
    simulated: "The simulated printer and scanner need no driver.",
    driverLabel: "Printer driver",
    backendLabel: "Scanner driver",
    automatic: (family: string) => `Automatic: ${family}`,
    automaticUnknown: "Automatic",
    automaticNone: "Automatic: no driver matches",
    ruleLabel: "Chosen by",
    noDriverHint:
      "Pick a driver family below, or finish setup and enter an exact driver name in Settings.",
    licencesTitle: "Vendor licences",
    noLicences: "These devices need no vendor files.",
  },
  review: {
    description: "Check the settings, then save. pagewire writes them to its settings file.",
    title: "Installation name",
    printers: "Printers",
    scanners: "Scanners",
    none: "None",
    simulatedDevice: "Simulated",
    notUsed: "Not used",
    noDriverHint: "After setup, choose a driver in Settings.",
    licences: "Accepted licences",
    noLicences: "None",
    finish: "Save",
    finishing: "Saving…",
    invalid: "The server rejected some settings:",
    conflict: "Another session finished setup. Reloading…",
    addConflict:
      "Someone else changed the settings at the same time. Save again to add the devices to the current settings.",
    reloadNow: "Reload now",
    saveAgain: "Save again",
  },
  applying: {
    title: "Starting the devices",
    description: "pagewire saved the settings and is starting the printers and scanners.",
    progress: "Starting",
    timeout:
      "This is taking longer than 3 minutes. The Status page shows what each device is doing.",
    goToStatus: "Go to Status",
  },
  defaults: {
    description:
      "What new print and scan jobs start with, from what each device offers. You can change them per job and later in Settings.",
    keep: "Keep these",
    save: "Save",
    saving: "Saving…",
    skip: "Skip",
    conflict:
      "Someone else changed the settings at the same time. Save again to apply these defaults on top of their changes.",
    saveAgain: "Save again",
  },
  done: {
    title: "pagewire is ready",
    description: "Print and scan from this page, or add the devices on your computers and phones.",
    networkName: "Name on your network",
    printerAddress: "Printer address",
    scannerAddress: "Scanner address",
    connectHint:
      "Most computers and phones find the devices by themselves. Use these addresses when they do not, for example over a VPN.",
    goToStatus: "Go to Status",
  },
};
