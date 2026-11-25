// Picks one of several printers or scanners on Print and Scan: segments for up to four, a select
// beyond that, nothing for one. Each choice shows the name, the location and a status dot.
import { useId } from "react";
import * as RAC from "react-aria-components";
import type { StatusPrinter } from "../gen/api";
import type { DeviceKind } from "../lib/devices";
import { strings } from "../strings";
import { devicePickerStrings as t } from "./DevicePicker.strings";
import { phaseTone, Select } from "./ui";
import "./DevicePicker.css";

export type PickerDevice = Pick<StatusPrinter, "id" | "name" | "location" | "phase">;

const MAX_SEGMENTS = 4;

function Choice({ device }: { device: PickerDevice }) {
  return (
    <span className="device-picker-choice">
      <span className="device-picker-dot" data-tone={phaseTone(device.phase)} aria-hidden="true" />
      <span className="device-picker-text">
        <span className="device-picker-name">{device.name}</span>
        {/* The hidden commas keep the parts apart in the accessible name; the last one is the dot. */}
        {device.location && (
          <span className="device-picker-location">
            <span className="visually-hidden">, </span>
            {device.location}
          </span>
        )}
        <span className="visually-hidden">, {strings.devicePhase[device.phase]}</span>
      </span>
    </span>
  );
}

export function DevicePicker({
  kind,
  devices,
  selected,
  onSelect,
}: {
  kind: DeviceKind;
  devices: PickerDevice[];
  selected: string | undefined;
  onSelect: (id: string) => void;
}) {
  const labelId = useId();
  if (devices.length < 2) return null;
  const label = t[kind];

  if (devices.length > MAX_SEGMENTS) {
    return (
      <Select
        className="device-picker"
        label={label}
        selectedKey={selected ?? null}
        onSelectionChange={(key) => key !== null && onSelect(String(key))}
        options={devices.map((d) => ({
          id: d.id,
          label: <Choice device={d} />,
          textValue: d.location ? `${d.name}, ${d.location}` : d.name,
        }))}
      />
    );
  }

  return (
    <div className="ui-field device-picker">
      <span id={labelId} className="ui-label">
        {label}
      </span>
      <RAC.ToggleButtonGroup
        aria-labelledby={labelId}
        className="device-picker-segments"
        selectionMode="single"
        disallowEmptySelection
        selectedKeys={selected === undefined ? [] : [selected]}
        onSelectionChange={(keys) => {
          const [key] = [...keys];
          if (key !== undefined) onSelect(String(key));
        }}
      >
        {devices.map((d) => (
          <RAC.ToggleButton key={d.id} id={d.id} className="device-picker-segment">
            <Choice device={d} />
          </RAC.ToggleButton>
        ))}
      </RAC.ToggleButtonGroup>
    </div>
  );
}
