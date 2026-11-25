import { afterEach, describe, expect, test } from "bun:test";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { useState } from "react";
import { DevicePicker, type PickerDevice } from "./DevicePicker";

afterEach(cleanup);

// happy-dom spaces out inline children when it computes a name; browsers join them as written.
const named = (name: string) => (accessibleName: string) =>
  accessibleName.replace(/ ,/g, ",") === name;

const devices: PickerDevice[] = [
  { id: "hp", name: "HP OfficeJet", location: "Study", phase: "ready" },
  { id: "samsung", name: "Samsung SCX-4300", location: "", phase: "error" },
  { id: "canon", name: "Canon", location: "Hall", phase: "deploying" },
  { id: "epson", name: "Epson", location: "", phase: "none" },
  { id: "brother", name: "Brother", location: "Loft", phase: "degraded" },
];

function Harness({ list, initial }: { list: PickerDevice[]; initial: string }) {
  const [selected, setSelected] = useState(initial);
  return (
    <>
      <DevicePicker kind="printer" devices={list} selected={selected} onSelect={setSelected} />
      <output>{selected}</output>
    </>
  );
}

describe("DevicePicker", () => {
  test("renders nothing for no device or one device", () => {
    const { container } = render(
      <>
        <DevicePicker kind="printer" devices={[]} selected={undefined} onSelect={() => {}} />
        <DevicePicker
          kind="scanner"
          devices={devices.slice(0, 1)}
          selected="hp"
          onSelect={() => {}}
        />
      </>,
    );
    expect(container.innerHTML).toBe("");
  });

  test("two to four devices are a labelled group of segments; one is checked", () => {
    render(<Harness list={devices.slice(0, 4)} initial="samsung" />);
    const group = screen.getByRole("radiogroup", { name: "Printer" });
    const segments = within(group).getAllByRole("radio");
    expect(segments).toHaveLength(4);
    // Name, location and the dot's meaning, kept apart in the accessible name.
    expect(screen.getByRole("radio", { name: named("HP OfficeJet, Study, Ready") })).toBeTruthy();
    const samsung = screen.getByRole("radio", { name: named("Samsung SCX-4300, Error") });
    expect(samsung.getAttribute("aria-checked")).toBe("true");
    expect(samsung.querySelector('.device-picker-dot[data-tone="problem"]')).toBeTruthy();
    expect(
      screen.getByRole("radio", { name: /^Canon/ }).querySelector('[data-tone="busy"]'),
    ).toBeTruthy();

    fireEvent.click(screen.getByRole("radio", { name: /^Canon/ }));
    expect(screen.getByRole("status").textContent).toBe("canon");
    // The selected segment stays selected when pressed again.
    fireEvent.click(screen.getByRole("radio", { name: /^Canon/ }));
    expect(screen.getByRole("status").textContent).toBe("canon");
  });

  test("more than four devices are a select showing the selected device", () => {
    render(<Harness list={devices} initial="brother" />);
    expect(screen.queryByRole("radiogroup")).toBeNull();
    const trigger = screen.getByRole("button", { name: /Printer/ });
    expect(trigger.getAttribute("aria-haspopup")).toBe("listbox");
    expect(trigger.querySelector(".ui-select-value")?.textContent).toBe("Brother, Loft");
  });
});
