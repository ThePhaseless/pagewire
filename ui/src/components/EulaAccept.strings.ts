// Copy for EulaAccept, shared by the setup wizard and Settings.

export const eulaStrings = {
  meta: (vendor: string, version: string) => `${vendor}, version ${version}`,
  required: "Needed by this printer",
  optional: "Optional",
  loading: "Loading the licence…",
  loadFailed: "Could not load the licence.",
  pending: (vendor: string) => `Downloading the file from ${vendor} and checking its signature…`,
  failed: (title: string) =>
    `printscan could not read the licence, so it cannot install ${title}. Check that the server can reach the internet. printscan tries again after 5 minutes, then less often while it keeps failing.`,
  textLabel: (title: string) => `${title} licence text`,
  changed:
    "The licence text has changed since you accepted it. Read it again. printscan stops using these files until you accept the new text.",
  accept: (title: string, version: string) =>
    `I accept the ${title} licence shown above, version ${version}`,
  howItWorks: (vendor: string) =>
    `printscan downloaded this file from ${vendor} and checked its signature to show you the licence. It installs the file only after you accept, and only if the licence text still matches.`,
};
