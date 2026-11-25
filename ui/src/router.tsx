// A small history router: five fixed routes, link interception, focus management and the page title.
import { useEffect, useSyncExternalStore } from "react";

export const routes = ["/", "/print", "/scan", "/settings", "/setup"] as const;
export type Route = (typeof routes)[number];

const NAVIGATE = "printscan:navigate";

export function isRoute(path: string): path is Route {
  return (routes as readonly string[]).includes(path);
}

/**
 * Decides whether a navigation away from the page may happen now. Returning true holds it; the
 * blocker then calls `proceed` to let it through (after asking, for example).
 */
export type Blocker = (proceed: () => void) => boolean;

let blocker: { fn: Blocker; href: string } | null = null;

/** Holds navigations to another page while `fn` is set (unsaved edits, for example). */
export function useNavigationBlocker(fn: Blocker | null) {
  useEffect(() => {
    if (!fn) return;
    const mine = { fn, href: window.location.href };
    blocker = mine;
    return () => {
      if (blocker === mine) blocker = null;
    };
  }, [fn]);
}

// Registered before any subscriber, so a held Back or Forward never reaches the router.
if (typeof window !== "undefined") {
  window.addEventListener("popstate", (e) => {
    const held = blocker;
    if (!held || new URL(held.href).pathname === window.location.pathname) return;
    const target = window.location.href;
    // The browser already moved the address; put it back and ask.
    window.history.pushState(null, "", held.href);
    e.stopImmediatePropagation();
    if (!held.fn(() => navigate(target, { force: true }))) navigate(target, { force: true });
  });
}

export function navigate(to: string, opts: { replace?: boolean; force?: boolean } = {}) {
  const url = new URL(to, window.location.href);
  if (
    url.pathname + url.search + url.hash ===
    window.location.pathname + window.location.search + window.location.hash
  ) {
    return;
  }
  const otherPage = url.pathname + url.search !== window.location.pathname + window.location.search;
  if (otherPage && !opts.force && blocker?.fn(() => navigate(to, { ...opts, force: true }))) return;
  if (opts.replace) window.history.replaceState(null, "", url);
  else window.history.pushState(null, "", url);
  window.dispatchEvent(new Event(NAVIGATE));
}

function subscribe(fn: () => void) {
  window.addEventListener("popstate", fn);
  window.addEventListener(NAVIGATE, fn);
  return () => {
    window.removeEventListener("popstate", fn);
    window.removeEventListener(NAVIGATE, fn);
  };
}

export function usePath(): string {
  return useSyncExternalStore(subscribe, () => window.location.pathname);
}

/** Turns clicks on same-origin links to app routes into history navigation. */
export function useLinkInterception() {
  useEffect(() => {
    const onClick = (e: MouseEvent) => {
      if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey)
        return;
      const a = (e.target as Element | null)?.closest?.("a");
      if (!a || a.target || a.hasAttribute("download")) return;
      const url = new URL(a.href, window.location.href);
      if (url.origin !== window.location.origin || !isRoute(url.pathname)) return;
      // In-page anchors (the skip link) keep their native behaviour.
      if (url.pathname === window.location.pathname && url.hash) return;
      e.preventDefault();
      navigate(url.pathname + url.search + url.hash);
    };
    document.addEventListener("click", onClick);
    return () => document.removeEventListener("click", onClick);
  }, []);
}

/** After a navigation (not the first render), moves focus to the page heading and scrolls up. */
export function useFocusOnNavigate() {
  useEffect(() => {
    const onNavigate = () => {
      requestAnimationFrame(() => {
        const h1 = document.querySelector<HTMLElement>("main h1");
        if (h1) {
          if (!h1.hasAttribute("tabindex")) h1.setAttribute("tabindex", "-1");
          h1.focus({ preventScroll: true });
        }
        window.scrollTo({ top: 0 });
      });
    };
    window.addEventListener(NAVIGATE, onNavigate);
    window.addEventListener("popstate", onNavigate);
    return () => {
      window.removeEventListener(NAVIGATE, onNavigate);
      window.removeEventListener("popstate", onNavigate);
    };
  }, []);
}

/**
 * Keeps document.title as "<page h1> · <suffix>". A heading's data-document-title (PageHeader's
 * documentTitle) replaces its text. Views change their h1 without a navigation event (data loads,
 * wizard steps), so this watches the main element instead of the router.
 */
export function useDocumentTitle(suffix: string) {
  useEffect(() => {
    const main = document.querySelector("main");
    if (!main) return;
    const update = () => {
      const h1 = main.querySelector<HTMLElement>("h1");
      const heading = h1?.dataset.documentTitle ?? h1?.textContent?.trim();
      const title = heading ? `${heading} · ${suffix}` : suffix;
      if (document.title !== title) document.title = title;
    };
    update();
    const observer = new MutationObserver(update);
    observer.observe(main, {
      subtree: true,
      childList: true,
      characterData: true,
      attributeFilter: ["data-document-title"],
    });
    return () => observer.disconnect();
  }, [suffix]);
}
