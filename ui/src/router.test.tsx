import { afterEach, describe, expect, test } from "bun:test";
import { act, cleanup, fireEvent, render, renderHook, waitFor } from "@testing-library/react";
import { useState } from "react";
import { PageHeader } from "./components/ui";
import {
  navigate,
  replaceQueryParam,
  useDocumentTitle,
  useFocusOnNavigate,
  useNavigationBlocker,
  useQueryParam,
} from "./router";

afterEach(cleanup);

let setHeading: (h: { title: string; documentTitle?: string }) => void = () => {};
let setName: (n: string) => void = () => {};

function Harness() {
  const [heading, setH] = useState<{ title: string; documentTitle?: string }>({ title: "Status" });
  const [name, setN] = useState("pagewire");
  setHeading = setH;
  setName = setN;
  useDocumentTitle(name);
  return (
    <main>
      <PageHeader title={heading.title} documentTitle={heading.documentTitle} />
    </main>
  );
}

describe("document title", () => {
  test("is the page heading and the device name on first render", () => {
    render(<Harness />);
    expect(document.title).toBe("Status · pagewire");
  });

  test("follows heading changes, the setup step override and the device name", async () => {
    render(<Harness />);
    act(() => setHeading({ title: "Print" }));
    await waitFor(() => expect(document.title).toBe("Print · pagewire"));
    act(() => setName("Hall printer"));
    await waitFor(() => expect(document.title).toBe("Print · Hall printer"));
    act(() =>
      setHeading({
        title: "Name and location",
        documentTitle: "Step 2 of 5: Name and location",
      }),
    );
    await waitFor(() =>
      expect(document.title).toBe("Step 2 of 5: Name and location · Hall printer"),
    );
  });
});

function Blocking({ onBlock }: { onBlock: (proceed: () => void) => void }) {
  useNavigationBlocker((proceed) => {
    onBlock(proceed);
    return true;
  });
  return null;
}

describe("navigation blocker", () => {
  afterEach(() => window.history.replaceState(null, "", "/"));

  test("holds navigate() until proceed, and lets hash-only moves and forced moves through", () => {
    window.history.replaceState(null, "", "/settings");
    let proceed: (() => void) | null = null;
    render(<Blocking onBlock={(p) => (proceed = p)} />);
    act(() => navigate("/print"));
    expect(window.location.pathname).toBe("/settings");
    expect(proceed).not.toBeNull();
    act(() => navigate("#general"));
    expect(window.location.hash).toBe("#general");
    act(() => proceed?.());
    expect(window.location.pathname).toBe("/print");
    act(() => navigate("/setup", { force: true }));
    expect(window.location.pathname).toBe("/setup");
  });

  test("the back button is held too, and the address returns to the page", () => {
    window.history.replaceState(null, "", "/print");
    window.history.pushState(null, "", "/settings");
    let proceed: (() => void) | null = null;
    const seen: string[] = [];
    const onPop = () => seen.push(window.location.pathname);
    render(<Blocking onBlock={(p) => (proceed = p)} />);
    window.addEventListener("popstate", onPop);
    // What the browser does on Back: the address changes, then popstate fires.
    window.history.replaceState(null, "", "/print");
    act(() => {
      window.dispatchEvent(new PopStateEvent("popstate"));
    });
    window.removeEventListener("popstate", onPop);
    expect(window.location.pathname).toBe("/settings");
    // Later listeners (the router's subscribers) never saw the blocked move.
    expect(seen).toEqual([]);
    act(() => proceed?.());
    expect(window.location.pathname).toBe("/print");
  });

  test("nothing is held once the blocker unmounts", () => {
    window.history.replaceState(null, "", "/settings");
    const view = render(<Blocking onBlock={() => {}} />);
    view.unmount();
    act(() => navigate("/scan"));
    expect(window.location.pathname).toBe("/scan");
  });
});

function FocusHarness() {
  useFocusOnNavigate();
  return (
    <main>
      <h1>Step</h1>
      <input aria-label="Name" />
      <button type="button">Next</button>
    </main>
  );
}

const nextFrame = () => new Promise((r) => requestAnimationFrame(r));

/** A Tab press that moves focus to `to` (null: off the page), as the browser does it. */
function tab(to: HTMLElement | null) {
  fireEvent.keyDown(document.activeElement ?? document.body, { key: "Tab" });
  if (to) to.focus();
  else (document.activeElement as HTMLElement | null)?.blur();
}

describe("focus after navigation", () => {
  afterEach(() => window.history.replaceState(null, "", "/"));

  test("moves to the page heading", async () => {
    const { getByRole } = render(<FocusHarness />);
    getByRole("button").focus();
    act(() => navigate("#two"));
    await nextFrame();
    expect(document.activeElement).toBe(getByRole("heading"));
  });

  test("leaves focus the user moved before the frame where it is", async () => {
    const { getByRole } = render(<FocusHarness />);
    getByRole("button").focus();
    act(() => navigate("#two"));
    // A fast keyboard user (or a test) tabs on before the next frame runs.
    tab(getByRole("textbox"));
    await nextFrame();
    expect(document.activeElement).toBe(getByRole("textbox"));
  });

  test("leaves focus that went away and came back to the pressed control", async () => {
    const { getByRole } = render(<FocusHarness />);
    getByRole("button").focus();
    act(() => navigate("#two"));
    // Tab wraps round the new page to the same Next button, ready for Enter.
    tab(getByRole("textbox"));
    tab(getByRole("button"));
    await nextFrame();
    expect(document.activeElement).toBe(getByRole("button"));
  });

  test("leaves focus that tabbed past the last control on the body", async () => {
    const { getByRole } = render(<FocusHarness />);
    getByRole("button").focus();
    act(() => navigate("#two"));
    tab(null);
    await nextFrame();
    expect(document.activeElement).toBe(document.body);
  });

  test("still moves to the heading when the focused control unmounted", async () => {
    const { getByRole } = render(<FocusHarness />);
    const gone = document.createElement("button");
    document.body.append(gone);
    gone.focus();
    act(() => navigate("#two"));
    gone.remove();
    await nextFrame();
    expect(document.activeElement).toBe(getByRole("heading"));
  });

  test("still moves to the heading when the app moved focus itself", async () => {
    const { getByRole } = render(<FocusHarness />);
    getByRole("button").focus();
    act(() => navigate("#two"));
    // A closing dialog hands focus back to the control that opened it.
    getByRole("textbox").focus();
    await nextFrame();
    expect(document.activeElement).toBe(getByRole("heading"));
  });
});

describe("query parameters", () => {
  afterEach(() => window.history.replaceState(null, "", "/"));

  test("useQueryParam reads the address and follows navigation", () => {
    window.history.replaceState(null, "", "/print?printer=hp");
    const { result } = renderHook(() => useQueryParam("printer"));
    expect(result.current).toBe("hp");
    act(() => navigate("/print?printer=canon%20lide"));
    expect(result.current).toBe("canon lide");
    act(() => navigate("/print"));
    expect(result.current).toBeNull();
  });

  test("replaceQueryParam changes the address in place: no history entry, no focus move", async () => {
    window.history.replaceState(null, "", "/scan?scanner=hp#top");
    const { getByRole } = render(<FocusHarness />);
    const { result } = renderHook(() => useQueryParam("scanner"));
    getByRole("button").focus();
    const length = window.history.length;
    act(() => replaceQueryParam("scanner", "lide"));
    expect(window.location.pathname + window.location.search + window.location.hash).toBe(
      "/scan?scanner=lide#top",
    );
    expect(result.current).toBe("lide");
    act(() => replaceQueryParam("scanner", null));
    expect(window.location.search).toBe("");
    expect(result.current).toBeNull();
    expect(window.history.length).toBe(length);
    await nextFrame();
    expect(document.activeElement).toBe(getByRole("button"));
  });
});
