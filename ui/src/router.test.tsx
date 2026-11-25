import { afterEach, describe, expect, test } from "bun:test";
import { act, cleanup, render, waitFor } from "@testing-library/react";
import { useState } from "react";
import { PageHeader } from "./components/ui";
import { navigate, useDocumentTitle, useNavigationBlocker } from "./router";

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
