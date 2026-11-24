// Consent to one vendor licence (catalog EULA). Controlled, so Settings can also revoke with it.
import type { SetupEULA } from "../gen/api";
import { eulaStrings } from "./EulaAccept.strings";
import { Checkbox } from "./ui";

export function EulaAccept({
  eula,
  isAccepted,
  onChange,
}: {
  eula: SetupEULA;
  isAccepted: boolean;
  onChange: (accepted: boolean) => void;
}) {
  // The URL comes from the catalog; only web links are rendered.
  const href = /^https?:\/\//i.test(eula.url) ? eula.url : null;
  return (
    <div className="ui-field">
      <h3>{eula.title}</h3>
      <p className="ui-field-description">
        {eulaStrings.meta(eula.vendor, eula.version)} ·{" "}
        {eula.required ? eulaStrings.required : eulaStrings.optional}
      </p>
      <p>{eula.summary}</p>
      {href && (
        <p>
          <a href={href} target="_blank" rel="noopener noreferrer">
            {eulaStrings.readLicence}
          </a>
        </p>
      )}
      <Checkbox isSelected={isAccepted} onChange={onChange}>
        {eulaStrings.accept(eula.title)}
      </Checkbox>
      <p className="ui-field-description">{eulaStrings.nothingDownloaded}</p>
    </div>
  );
}
