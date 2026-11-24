// Copy for the Print view. Shared words (the page title, problem messages) come from ../strings.

export const printStrings = {
  description: "Send documents to the printer. Each file prints as its own job.",
  unavailable: {
    loading: "Checking the printer…",
    title: "The printer is not ready",
    waiting: "Printing becomes available as soon as the printer is ready.",
    seeStatus: "See Status",
  },
  files: {
    title: "Documents",
    drop: "Drop files here",
    or: "or",
    choose: "Choose files",
    accepted: (list: string) => `This printer accepts ${list}.`,
    acceptsNothing: "The printer does not list any document format it accepts.",
    limit: (mib: number) => `Up to ${mib} MiB per file.`,
    checking: "Checking the file…",
    ready: "Ready to print",
    size: (bytes: number) =>
      bytes < 1024 * 1024
        ? `${Math.max(1, Math.round(bytes / 1024))} KB`
        : `${(bytes / (1024 * 1024)).toFixed(1)} MB`,
    unsupported: (format: string) => `This printer does not accept ${format} files.`,
    unknownFormat:
      "This is not a document format printscan can send. Save it as one of the accepted formats.",
    tooLarge: (mib: number) => `Larger than the ${mib} MiB upload limit in Settings.`,
    readError: "This file could not be read.",
    uploading: (name: string) => `Uploading ${name}`,
    remove: (name: string) => `Remove ${name}`,
  },
  formats: {
    "application/pdf": "PDF",
    "application/postscript": "PostScript",
    "image/jpeg": "JPEG",
    "image/png": "PNG",
    "image/pwg-raster": "PWG raster",
    "image/urf": "Apple raster",
    "text/plain": "plain text",
  } as Record<string, string>,
  options: {
    title: "Options",
    copies: "Copies",
    copiesError: "Enter 1 to 99 copies.",
    sides: {
      label: "Sides",
      "one-sided": "One-sided",
      "two-sided-long-edge": "Two-sided, flip on long edge",
      "two-sided-short-edge": "Two-sided, flip on short edge",
    },
    colorMode: {
      label: "Colour",
      auto: "Automatic",
      color: "Colour",
      monochrome: "Black and white",
    },
    media: "Paper size",
    mediaSize: (name: string, width: string, height: string, unit: string) =>
      `${name} (${width} × ${height} ${unit})`,
    quality: {
      label: "Quality",
      draft: "Draft",
      normal: "Normal",
      high: "High",
    },
  },
  submit: (files: number, copies: number) =>
    files > 1 ? `Print ${files} files` : copies > 1 ? `Print ${copies} copies` : "Print",
  sent: (name: string) => `Sent ${name} to the printer`,
  failed: (name: string) => `Could not print ${name}`,
  jobs: {
    title: "Print jobs",
    empty: "No print jobs yet",
    emptyBody: "Jobs sent from this page or from other devices show up here.",
    unavailable: "Jobs appear here once the printer is ready.",
    cancel: "Cancel",
    cancelJob: (name: string) => `Cancel ${name}`,
    cancelFailed: (name: string) => `Could not cancel ${name}`,
    state: {
      pending: "Waiting",
      processing: "Printing",
      completed: "Printed",
      canceled: "Canceled",
      aborted: "Failed",
      held: "Held",
      stopped: "Stopped",
    },
  },
} as const;
