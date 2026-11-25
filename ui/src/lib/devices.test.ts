import { afterEach, beforeEach, describe, expect, test } from "bun:test";
import { act, renderHook } from "@testing-library/react";
import { navigate } from "../router";
import { enabled, pickRemembered, remember, useDeviceChoice } from "./devices";

const list = [{ id: "hp" }, { id: "samsung" }, { id: "canon" }];

beforeEach(() => localStorage.clear());
afterEach(() => window.history.replaceState(null, "", "/"));

describe("enabled", () => {
  test("keeps the switched-on entries in order", () => {
    const all = [
      { id: "a", enabled: true },
      { id: "b", enabled: false },
      { id: "c", enabled: true },
    ];
    expect(enabled(all).map((d) => d.id)).toEqual(["a", "c"]);
    expect(enabled([])).toEqual([]);
  });
});

describe("pickRemembered", () => {
  test("defaults to the first device and stores nothing", () => {
    expect(pickRemembered(list, "printer", null)?.id).toBe("hp");
    expect(localStorage.getItem("pagewire:printer")).toBeNull();
  });

  test("a query selects a device and remembers it, per kind", () => {
    expect(pickRemembered(list, "printer", "samsung")?.id).toBe("samsung");
    expect(localStorage.getItem("pagewire:printer")).toBe("samsung");
    expect(localStorage.getItem("pagewire:scanner")).toBeNull();
    expect(pickRemembered(list, "printer", null)?.id).toBe("samsung");
    expect(pickRemembered(list, "scanner", null)?.id).toBe("hp");
  });

  test("an unknown query or remembered id falls back without overwriting the remembered one", () => {
    remember("scanner", "canon");
    expect(pickRemembered(list, "scanner", "gone")?.id).toBe("canon");
    expect(pickRemembered([{ id: "hp" }], "scanner", null)?.id).toBe("hp");
    expect(localStorage.getItem("pagewire:scanner")).toBe("canon");
    // Before the first Status the list is empty: nothing to show, nothing forgotten.
    expect(pickRemembered([], "scanner", "hp")).toBeUndefined();
    expect(localStorage.getItem("pagewire:scanner")).toBe("canon");
  });

  test("works when storage throws", () => {
    const real = Object.getOwnPropertyDescriptor(window, "localStorage");
    Object.defineProperty(window, "localStorage", {
      configurable: true,
      get() {
        throw new Error("blocked");
      },
    });
    try {
      expect(pickRemembered(list, "printer", "canon")?.id).toBe("canon");
      expect(pickRemembered(list, "printer", null)?.id).toBe("hp");
      expect(() => remember("printer", "hp")).not.toThrow();
    } finally {
      if (real) Object.defineProperty(window, "localStorage", real);
    }
  });
});

describe("useDeviceChoice", () => {
  test("reads the query, and a choice is remembered and drops the query", () => {
    window.history.replaceState(null, "", "/print?printer=canon&x=1");
    const { result } = renderHook(() => useDeviceChoice("printer", list));
    expect(result.current[0]?.id).toBe("canon");
    act(() => result.current[1]("samsung"));
    expect(result.current[0]?.id).toBe("samsung");
    expect(localStorage.getItem("pagewire:printer")).toBe("samsung");
    expect(window.location.search).toBe("?x=1");
  });

  test("a later link with a query wins over an earlier choice", () => {
    window.history.replaceState(null, "", "/print");
    const { result } = renderHook(() => useDeviceChoice("printer", list));
    act(() => result.current[1]("samsung"));
    act(() => navigate("/print?printer=hp"));
    expect(result.current[0]?.id).toBe("hp");
  });
});
