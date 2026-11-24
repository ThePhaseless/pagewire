// A small history router: five fixed routes, link interception and focus management.
import { useEffect, useSyncExternalStore } from "react";

export const routes = ["/", "/print", "/scan", "/settings", "/setup"] as const;
export type Route = (typeof routes)[number];

const NAVIGATE = "printscan:navigate";

export function isRoute(path: string): path is Route {
  return (routes as readonly string[]).includes(path);
}

export function navigate(to: string, opts: { replace?: boolean } = {}) {
  const url = new URL(to, window.location.href);
  if (
    url.pathname + url.search + url.hash ===
    window.location.pathname + window.location.search + window.location.hash
  ) {
    return;
  }
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
