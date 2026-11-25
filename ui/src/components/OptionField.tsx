// The one control for every option kind except copies (A23), on Print, Scan, Settings and the
// setup wizard. Job mode hides what the device cannot change; defaults mode shows every kind,
// greys out the unchangeable ones with the reason as the accessible description, and offers the
// device's default first, which removes the config key.
import type { Key } from "react-aria-components";
import { type DeviceRole, optionLabel, optionTitle } from "../lib/labels";
import {
  type FieldKind,
  isNumeric,
  type OptionState,
  type OptionValue,
  preselect,
} from "../lib/options";
import { optionFieldStrings as t } from "./OptionField.strings";
import { Select } from "./ui";

/** The id of the device-default entry in defaults mode. */
// shortcut: an open keyword literally "default" (mediaSource, mediaType, outputBin) would share this id; prefix value ids if a printer ever reports one.
export const DEFAULT_ID = "default";

export type OptionFieldProps = {
  kind: FieldKind;
  device: DeviceRole;
  state: OptionState;
  /** The chosen (job) or saved (defaults) value; "" or 0 is "no saved value". */
  value: OptionValue | undefined;
  /** Gets "" or 0 when the default entry is chosen. */
  onChange: (value: OptionValue) => void;
  mode: "job" | "defaults";
  errorMessage?: string;
};

export function OptionField({
  kind,
  device,
  state,
  value,
  onChange,
  mode,
  errorMessage,
}: OptionFieldProps) {
  const label = (v: OptionValue) => optionLabel(kind, v, device);
  const option = (v: OptionValue) => ({ id: String(v), label: label(v) });

  if (mode === "job") {
    if (state.reason) return null;
    const selected = preselect(kind, state, value);
    return (
      <Select
        label={optionTitle(kind, device)}
        options={state.values.map(option)}
        selectedKey={selected === undefined ? null : String(selected)}
        onSelectionChange={(k: Key | null) => {
          const v = state.values.find((x) => String(x) === k);
          if (v !== undefined) onChange(v);
        }}
        description={kind === "format" ? t.formatHint : undefined}
        errorMessage={errorMessage}
      />
    );
  }

  const saved = value === undefined || value === "" || value === 0 ? undefined : value;
  const offered = saved !== undefined && state.values.includes(saved);
  // Known caps without the saved value: show the default entry, which the next save writes.
  const notOffered = saved !== undefined && !offered && state.known;
  // Unknown caps: keep showing the saved value.
  const values =
    saved !== undefined && !offered && !state.known ? [...state.values, saved] : state.values;
  const description = [state.reason, notOffered && t.savedNotOffered(device, label(saved))]
    .filter(Boolean)
    .join(" ");
  const def = state.deviceDefault;
  return (
    <Select
      label={optionTitle(kind, device)}
      options={[
        {
          id: DEFAULT_ID,
          label: t.deviceDefault(device, def === undefined ? undefined : label(def)),
        },
        ...values.map(option),
      ]}
      selectedKey={saved !== undefined && !notOffered ? String(saved) : DEFAULT_ID}
      onSelectionChange={(k: Key | null) => {
        if (k === DEFAULT_ID) return onChange(isNumeric(kind) ? 0 : "");
        const v = values.find((x) => String(x) === k);
        if (v !== undefined) onChange(v);
      }}
      isDisabled={state.reason !== null}
      description={description || undefined}
      errorMessage={errorMessage}
    />
  );
}
