// Copy for the Status view. Shared words (Copy, Copied, the page title, device phases, driver
// families and rules) come from ../strings.

export const statusStrings = {
  description:
    "What your printers and scanners are doing, and how to reach them from other devices.",
  workloadError: "A device workload has a problem",
  workloadOn: (node: string) => `Devices on ${node}`,
  empty: {
    title: "No printer or scanner in use",
    body: "Switch one on in Settings, or detect a newly plugged-in device and add it.",
    detect: "Detect and add devices",
  },
  device: {
    simulatedDevice: "Simulated device",
    unnamedDevice: "Unnamed USB device",
    deviceLabel: "Device",
    usbLabel: "USB",
    usbIds: (vendorId: string, productId: string) => `USB ${vendorId}:${productId}`,
    serial: (serial: string) => `serial ${serial}`,
    nodeLabel: "Node",
    ruleLabel: "Chosen by",
    printerFamilyLabel: "Printer driver",
    scannerFamilyLabel: "Scanner driver",
    saneBackendsLabel: "SANE backends",
    driverLabel: "Driver",
    appDriver: "Picked by the printer application",
    chooseDriver: "Choose a driver in Settings",
    noFamily: "None",
    warningsLabel: "Settings this device does not offer",
    blobsTitle: "Vendor files",
    blobState: {
      installed: "Installed",
      missing: "Not installed",
      failed: "Failed",
      disabled: "Not available",
    },
  },
  advertised: {
    title: "On your network",
    discoverable: "Discoverable on your network",
    notDiscoverable:
      "Not discoverable on your network yet. Other devices find it once it is running.",
    types: {
      "_ipp._tcp": "Printer",
      "_uscan._tcp": "Scanner",
    } as Record<string, string>,
    port: (port: number) => `port ${port}`,
  },
  printer: {
    state: {
      idle: "Ready",
      processing: "Printing",
      stopped: "Stopped",
      unknown: "Waiting for the printer",
    },
    reasonsLabel: "Printer reports",
    reasons: {
      "media-empty": "Out of paper",
      "media-needed": "Load paper",
      "media-jam": "Paper jam",
      "media-low": "Paper is running low",
      "toner-empty": "Out of toner",
      "toner-low": "Toner is running low",
      "marker-supply-empty": "Out of ink or toner",
      "marker-supply-low": "Ink or toner is running low",
      "door-open": "A door or cover is open",
      "cover-open": "A cover is open",
      "input-tray-missing": "A paper tray is missing",
      "output-area-full": "The output tray is full",
      offline: "Offline",
      paused: "Paused",
      shutdown: "Turned off",
      "connecting-to-device": "Connecting to the printer",
      "moving-to-paused": "Pausing",
      "spool-area-full": "The print queue is full",
    } as Record<string, string>,
    unavailable: "You can print here once the printer is running.",
    open: "Print a document",
    suppliesLabel: "Supplies",
    unnamedSupply: "Supply",
    levelUnknown: "level unknown",
  },
  scanner: {
    state: {
      idle: "Ready",
      processing: "Scanning",
      stopped: "Stopped",
      unknown: "Waiting for the scanner",
    },
    unavailable: "You can scan here once the scanner is running.",
    open: "Scan a document",
  },
  jobs: {
    title: "Recent print jobs",
    empty: "No print jobs yet",
    emptyBody: "Jobs sent from this page or from other devices show up here.",
    unavailable: "Jobs appear here once the printer is ready.",
    seeAll: "All jobs",
    seeAllOf: (name: string) => `All jobs on ${name}`,
    state: {
      pending: "Waiting",
      processing: "Printing",
      completed: "Sent to printer",
      canceled: "Canceled",
      aborted: "Failed",
      held: "Held",
      stopped: "Stopped",
    },
  },
  scans: {
    title: "Recent scans",
    empty: "No scans yet",
    emptyBody: "Scans you make on the Scan page are kept here for a while.",
    seeAll: "All scans",
    pages: (n: number) => (n === 1 ? "1 page" : `${n} pages`),
    state: {
      scanning: "Scanning",
      completed: "Done",
      failed: "Failed",
      canceled: "Canceled",
    },
  },
  system: {
    title: "System",
    configLabel: "Settings file",
  },
  config: {
    state: {
      missing: "No settings file",
      applied: "Applied",
      invalid: "Has errors",
    },
    changedAt: (when: string) => `Changed ${when}`,
    error: (line: number, column: number, message: string) =>
      `Line ${line}, column ${column}: ${message}`,
    openSettings: "Open Settings",
  },
  reconcile: {
    title: "Problems",
    items: {
      config: "Settings file",
      "config-migration": "Settings file update",
      "data-dir": "Data directory",
      nodes: "Nodes with devices",
      "backend-events": "Change notifications",
    } as Record<string, string>,
    /** `workload/<node>` items; `default` is the one workload of a single-node install. */
    workload: (node: string) => (node === "default" ? "Devices" : `Devices on ${node}`),
  },
  connect: {
    title: "Connect a device",
    printerIntro:
      "Phones, tablets and computers on the same network find this printer by themselves. Pick your system for the steps.",
    scannerIntro:
      "Phones, tablets and computers on the same network find this scanner by themselves. Pick your system for the steps.",
    nameLabel: "Advertised name",
    noName: "Not advertised yet",
    osLabel: "Your system",
    os: {
      windows: "Windows",
      apple: "macOS and iOS",
      android: "Android",
      linux: "Linux",
    },
    printerSteps: {
      windows: (name: string) => [
        "Open Settings, then Bluetooth & devices, then Printers & scanners, and choose Add device.",
        `Pick ${name} when it appears.`,
        "If it does not appear, choose Add manually, then Add a printer using an IP address or hostname, set the device type to IPP Device and paste the printer address below.",
      ],
      apple: (name: string) => [
        `On iPhone and iPad, choose Print from the share menu and pick ${name}. There is nothing to install.`,
        `On a Mac, open System Settings, then Printers & Scanners, click Add Printer, Scanner or Fax, and pick ${name}.`,
      ],
      android: (name: string) => [
        "Open a document or photo, choose Print, then Select a printer, then All printers.",
        `Pick ${name}. The built-in print service handles it without an app.`,
      ],
      linux: (name: string) => [
        `Most desktops list ${name} in their printer settings automatically.`,
        "Otherwise add it with: lpadmin -p pagewire -E -m everywhere -v followed by the printer address below.",
      ],
    },
    scannerSteps: {
      windows: (name: string) => [
        "Open Settings, then Bluetooth & devices, then Printers & scanners, and choose Add device.",
        `Pick ${name} when it appears. Windows 11 adds network scanners without a driver.`,
      ],
      apple: (name: string) => [
        `On a Mac, open System Settings, then Printers & Scanners, click Add Printer, Scanner or Fax, and pick ${name}.`,
        "Scan from the same settings pane or from Image Capture.",
      ],
      android: (_name: string) => [
        "Scan from this web page, or with a scanning app such as Mopria Scan.",
      ],
      linux: (name: string) => [
        `Scanning works through sane-airscan, which finds ${name} by itself, for example in Document Scanner or Skanlite.`,
      ],
    },
    thisPrinter: "the printer",
    thisScanner: "the scanner",
    manualTitle: "Manual addresses",
    printerAddress: "Printer address",
    scannerAddress: "Scanner address",
    noAddress: "Not available yet",
    copyLabel: (value: string) => `Copy ${value}`,
    copyFailed: "Could not copy. Select the address and copy it by hand.",
    tailscale:
      "Over Tailscale or another VPN, automatic discovery does not reach other devices. Add it with a manual address instead, using the host's VPN address.",
  },
} as const;
