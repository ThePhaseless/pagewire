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
    limit: (mib: number) => `Up to ${mib} MB per file.`,
    checking: "Checking the file…",
    ready: "Ready to print",
    size: (bytes: number) =>
      bytes < 1024 * 1024
        ? `${Math.max(1, Math.round(bytes / 1024))} KB`
        : `${(bytes / (1024 * 1024)).toFixed(1)} MB`,
    unsupported: (format: string) => `This printer does not accept ${format} files.`,
    unknownFormat:
      "This is not a document format pagewire can send. Save it as one of the accepted formats.",
    tooLarge: (mib: number) => `Larger than the ${mib} MB upload limit in Settings.`,
    readError: "This file could not be read.",
    uploading: (name: string) => `Uploading ${name}`,
    remove: (name: string) => `Remove ${name}`,
  },
  /** How to turn a refused file into one the printer takes, by kind of file. */
  help: {
    document: "Open it in Word or Google Docs, choose Print > Save as PDF, then add the PDF here.",
    photo: "Open it in your photo app and export it as JPEG, then add the JPEG here.",
    text: "Open it in a text editor, choose Print > Save as PDF, then add the PDF here.",
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
    copiesError: (max: number) => `Enter 1 to ${max} copies.`,
    more: "More options",
    unknown: "The options this printer offers show once it is connected.",
    none: "This printer offers no options to choose.",
  },
  /** The Print button: `printable` of the `total` files listed go out, `copies` of each. */
  submit: (printable: number, total: number, copies: number) => {
    if (total <= 1) return copies > 1 ? `Print ${copies} copies` : "Print";
    const files = printable < total ? `${printable} of ${total} files` : `${total} files`;
    return copies > 1 ? `Print ${files}, ${copies} copies each` : `Print ${files}`;
  },
  /** Copies, then the labels of the paper size and sides when they are sent. */
  summary: (copies: number, parts: string[]) =>
    [`${copies} ${copies === 1 ? "copy" : "copies"}`, ...parts].join(" · "),
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
      completed: "Sent to printer",
      canceled: "Canceled",
      aborted: "Failed",
      held: "Held",
      stopped: "Stopped",
    },
  },
} as const;
