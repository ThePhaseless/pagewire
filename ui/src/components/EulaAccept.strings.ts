// Copy for EulaAccept, shared by the setup wizard and Settings.

export const eulaStrings = {
  meta: (vendor: string, version: string) => `${vendor}, version ${version}`,
  required: "Needed by this printer",
  optional: "Optional",
  readLicence: "Read the licence (opens in a new tab)",
  accept: (title: string) => `I have read and accept the ${title} licence`,
  nothingDownloaded:
    "printscan downloads these vendor files only after you accept. Nothing is downloaded otherwise.",
};
