// Shared copy: the shell, navigation, banners, common buttons and problem messages.
// Each view keeps its own copy in views/<View>.strings.ts.

export const strings = {
  appName: "pagewire",
  skipToContent: "Skip to content",
  nav: {
    label: "Main",
    status: "Status",
    print: "Print",
    scan: "Scan",
    settings: "Settings",
    setup: "Set up",
  },
  connection: {
    ready: "Ready",
    working: "Starting",
    problem: "Needs attention",
    notFound: "Printer not found",
    idle: "Not set up",
    offline: "Offline",
  },
  banners: {
    offline: "Lost connection to pagewire. Reconnecting…",
    invalidConfig:
      "The settings file has an error, so pagewire keeps using the last working settings.",
    invalidConfigLine: (line: number, message: string) => `Line ${line}: ${message}`,
    openSettings: "Open Settings",
  },
  buttons: {
    save: "Save",
    cancel: "Cancel",
    close: "Close",
    back: "Back",
    next: "Next",
    retry: "Try again",
    delete: "Delete",
    dismiss: "Dismiss",
    copy: "Copy",
    copied: "Copied",
  },
  loading: "Loading…",
  technicalDetails: "Technical details",
  /** After the label of a field that must be filled in. */
  required: "(required)",
  /** Shown wherever the device is probing and finds no printer it can use. */
  plugInHint: "Plug the printer in with USB, switch it on and wait. This page updates by itself.",
  notFound: {
    title: "Page not found",
    body: "This address is not part of pagewire.",
    home: "Go to Status",
  },
  problems: {
    "config-missing": "There is no settings file yet. Finish setup to create it.",
    "storage-error":
      "pagewire could not write to its data directory. Check that the volume is mounted and writable.",
    "multipart-order": "The upload was malformed. Reload the page and try again.",
    "storage-full":
      "Stored scans have reached the 1 GiB limit. Delete old scans under Recent scans to scan again.",
    "invalid-image":
      "This image is damaged, or its contents do not match its type. Open it and save it again, or save it as a PDF.",
    "network-error": "Could not reach pagewire. Check your connection and try again.",
    "bad-response":
      "pagewire sent a response this page does not understand. Reload the page; if it persists, the UI and server versions differ.",
    byStatus: {
      400: "The request was not valid.",
      404: "That item no longer exists.",
      409: "That action conflicts with the current state. Refresh and try again.",
      412: "Someone else changed this in the meantime.",
      413: "The file is larger than the upload limit in Settings.",
      415: "The printer does not accept this file type.",
      422: "Some values are not valid.",
      428: "The request is missing its version check. Reload the page and try again.",
      500: "pagewire hit an internal error.",
      503: "The device is not available right now.",
    } as Record<number, string>,
    unknown: "Something went wrong.",
  },
} as const;
