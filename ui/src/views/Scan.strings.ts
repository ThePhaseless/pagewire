// Copy for the Scan view. Shared words (the page title, problem messages) come from ../strings.

export const scanStrings = {
  description: "Scan from the flatbed or the document feeder and download the result.",
  unavailable: {
    loading: "Checking the scanner…",
    title: "The scanner is not ready",
    waiting: "Scanning becomes available as soon as the scanner is ready.",
    seeStatus: "See Status",
  },
  options: {
    title: "Scan settings",
    loading: "Reading what the scanner can do…",
    cannotScan:
      "This scanner does not report a source, resolution or colour mode, so pagewire cannot scan with it.",
  },
  submit: "Scan",
  cancel: "Cancel scan",
  cancelFailed: "The scan was not canceled",
  ready: (pages: number) => `Scan ready, ${pages} ${pages === 1 ? "page" : "pages"}`,
  failed: "Scan failed",
  busy: "The scanner is busy. Wait for the current scan to finish.",
  scanning: (pages: number) =>
    pages === 0 ? "Scanning…" : `Scanning… ${pages} ${pages === 1 ? "page" : "pages"} so far`,
  result: {
    title: "Result",
    empty: "No scans yet",
    emptyBody: "Place a document on the glass or in the feeder and press Scan.",
    failed: "The scan failed",
    canceled: "The scan was canceled.",
    meta: (when: string, summary: string) => `${when} · ${summary}`,
    page: (n: number) => `Page ${n}`,
    openPage: (n: number) => `Open page ${n}`,
    pageOf: (n: number, total: number) => `Page ${n} of ${total}`,
    previous: "Previous page",
    next: "Next page",
  },
  download: {
    pdf: "Download PDF",
    jpg: "Download JPG",
    zip: "Download ZIP",
  },
  history: {
    title: "Recent scans",
    retention: (days: number) =>
      days === 0
        ? "Scans are kept until you delete them."
        : `Scans are deleted after ${days} ${days === 1 ? "day" : "days"}.`,
    /** A row's line: the scanner's name when it is known, then what it holds. */
    from: (scanner: string | undefined, summary: string) =>
      scanner ? `${scanner} · ${summary}` : summary,
    summary: (pages: number, format: string) =>
      `${pages} ${pages === 1 ? "page" : "pages"}, ${format}`,
    empty: "No recent scans",
    emptyBody: "Scans you make are listed here until you delete them or they expire.",
    show: "Show",
    showScan: (when: string) => `Show scan from ${when}`,
    deleteScan: (when: string) => `Delete scan from ${when}`,
    state: {
      scanning: "Scanning",
      completed: "Done",
      failed: "Failed",
      canceled: "Canceled",
    },
    loadFailed: "Could not load the scan list.",
  },
  confirmDelete: {
    title: "Delete this scan?",
    body: "The pages and the downloadable file are removed from pagewire. This cannot be undone.",
    confirm: "Delete scan",
    failed: "The scan was not deleted",
    deleted: "Scan deleted",
  },
} as const;
