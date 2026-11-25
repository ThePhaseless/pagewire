// Copy for OptionField and the option states in ../lib/options (A23). Value labels come from
// ../lib/labels.strings.

type Role = "printer" | "scanner";

export const optionFieldStrings = {
  notConnected: (device: Role) => `pagewire shows this ${device}'s options once it is connected.`,
  notReported: (device: Role) => `Not reported by this ${device}.`,
  oneSided: "This printer only prints one-sided.",
  /** `label` is already lower-case. */
  oneColorMode: (device: Role, label: string) =>
    device === "printer"
      ? `This printer only prints in ${label}.`
      : `This scanner only scans in ${label}.`,
  noFeeder: "This scanner has no document feeder.",
  noFlatbed: "This scanner has no flatbed.",
  onlyOffers: (device: Role, label: string) => `This ${device} only offers ${label}.`,
  /** The first entry in defaults mode; it removes the config key. */
  deviceDefault: (device: Role, label?: string) => {
    const name = device === "printer" ? "Printer default" : "Scanner default";
    return label ? `${name} (${label})` : name;
  },
  savedNotOffered: (device: Role, label: string) =>
    `Your saved “${label}” is not offered by this ${device}.`,
  formatHint: "PDF keeps all pages in one file; JPEG saves each page as an image",
};
