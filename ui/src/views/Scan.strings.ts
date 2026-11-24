// Copy for the Scan view. Shared words (the page title, problem messages) come from ../strings.

export const scanStrings = {
  description: "Scan from the glass or the document feeder and download the result.",
  unavailable: {
    loading: "Checking the scanner…",
    title: "The scanner is not ready",
    waiting: "Scanning becomes available as soon as the scanner is ready.",
    seeStatus: "See Status",
  },
  options: {
    title: "Scan settings",
    loading: "Reading what the scanner can do…",
    source: {
      label: "Source",
      platen: "Glass",
      adf: "Document feeder",
      "adf-duplex": "Document feeder, both sides",
    },
    resolution: "Resolution",
    dpi: (n: number) => `${n} dpi`,
    colorMode: {
      label: "Colour",
      color: "Colour",
      grayscale: "Greyscale",
      blackandwhite: "Black and white",
    },
    format: {
      label: "File type",
      pdf: "PDF",
      jpeg: "JPEG",
      pdfHint: "One document with every page",
      jpegHint: "One image per page; several pages download as a ZIP",
    },
  },
  submit: "Scan",
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
    summary: (pages: number, format: string) =>
      `${pages} ${pages === 1 ? "page" : "pages"}, ${format}`,
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
    body: "The pages and the downloadable file are removed from printscan. This cannot be undone.",
    confirm: "Delete scan",
    failed: "The scan was not deleted",
    deleted: "Scan deleted",
  },
} as const;
