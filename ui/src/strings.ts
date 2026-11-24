// Shared copy: the shell, navigation, banners, common buttons and problem messages.
// Each view keeps its own copy in views/<View>.strings.ts.

export const strings = {
  appName: "printscan",
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
    idle: "Not set up",
    offline: "Offline",
  },
  banners: {
    offline: "Lost connection to printscan. Reconnecting…",
    invalidConfig:
      "The config file has an error, so printscan keeps using the last working settings.",
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
  notFound: {
    title: "Page not found",
    body: "This address is not part of printscan.",
    home: "Go to Status",
  },
  problems: {
    "config-missing": "There is no config file yet. Finish setup to create it.",
    "storage-error":
      "printscan could not write to its data directory. Check that the volume is mounted and writable.",
    "multipart-order": "The upload was malformed. Reload the page and try again.",
    "network-error": "Could not reach printscan. Check your connection and try again.",
    "bad-response":
      "printscan sent a response this page does not understand. Reload the page; if it persists, the UI and server versions differ.",
    byStatus: {
      400: "The request was not valid.",
      404: "That item no longer exists.",
      409: "That action conflicts with the current state. Refresh and try again.",
      412: "Someone else changed this in the meantime.",
      413: "The file is larger than the upload limit in Settings.",
      415: "The printer does not accept this file type.",
      422: "Some values are not valid.",
      428: "The request is missing its version check. Reload the page and try again.",
      500: "printscan hit an internal error.",
      503: "The device is not available right now.",
    } as Record<number, string>,
    unknown: "Something went wrong.",
  },
} as const;
