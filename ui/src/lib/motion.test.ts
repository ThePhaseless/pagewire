import { describe, expect, test } from "bun:test";
import { renderHook } from "@testing-library/react";
import { createElement, type ReactNode } from "react";
import { createEventStore, EventsContext } from "../api/events";
import { useHasChanged, useJobsWithArrivals } from "./motion";

describe("useHasChanged", () => {
  const hook = (value: unknown, armed?: boolean) =>
    renderHook<boolean, { value: unknown; armed?: boolean }>(
      (p) => useHasChanged(p.value, p.armed),
      { initialProps: { value, armed } },
    );

  test("false on first render and after a rerender with the same value", () => {
    const { result, rerender } = hook("ok");
    expect(result.current).toBe(false);
    rerender({ value: "ok" });
    expect(result.current).toBe(false);
  });

  test("turns true after a different value, and stays true after returning", () => {
    const { result, rerender } = hook("ok");
    rerender({ value: "problem" });
    expect(result.current).toBe(true);
    rerender({ value: "ok" });
    expect(result.current).toBe(true);
  });

  test("a change while unarmed does not count; the baseline is taken at arming", () => {
    const { result, rerender } = hook("idle", false);
    rerender({ value: "ok", armed: false });
    expect(result.current).toBe(false);
    rerender({ value: "ok", armed: true });
    expect(result.current).toBe(false);
  });

  test("once armed it stays armed", () => {
    const a = hook("idle", false);
    a.rerender({ value: "ok", armed: true });
    a.rerender({ value: "problem", armed: true });
    expect(a.result.current).toBe(true);

    const b = hook("ok", true);
    b.rerender({ value: "idle", armed: false });
    expect(b.result.current).toBe(true);
  });
});

// The arrival cases run through the Status and Print view tests, which wire SSE and the API.
describe("useJobsWithArrivals", () => {
  test("no printer yet: nothing has arrived, and nothing throws", () => {
    const store = createEventStore({
      newEventSource: () => ({ close() {} }) as unknown as EventSource,
    });
    const wrapper = ({ children }: { children: ReactNode }) =>
      createElement(EventsContext, { value: store }, children);
    const { result } = renderHook(() => useJobsWithArrivals(undefined), { wrapper });
    expect(result.current.arrived(1)).toBe(false);
  });
});
