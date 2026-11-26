// The app shell: header with the installation name and live status, navigation, banners, routes
// and toasts.
import { type ReactNode, useEffect, useState } from "react";
import { RouterProvider } from "react-aria-components";
import { api } from "./api/client";
import {
  defaultStore,
  type EventStore,
  EventsContext,
  useConnected,
  useEnabledPrinters,
  useEnabledScanners,
  useNothingFound,
  useStatus,
} from "./api/events";
import {
  dismissProblemToasts,
  Icon,
  type IconName,
  StatusBadge,
  ToastRegion,
  type Tone,
} from "./components/ui";
import type { Status } from "./gen/api";
import {
  navigate,
  type Route,
  useDocumentTitle,
  useFocusOnNavigate,
  useLinkInterception,
  usePath,
} from "./router";
import { strings } from "./strings";
import { Print } from "./views/Print";
import { Scan } from "./views/Scan";
import { Settings } from "./views/Settings";
import { Setup } from "./views/Setup";
import { Status as StatusView } from "./views/Status";

export type Views = Record<Route, () => ReactNode>;

const defaultViews: Views = {
  "/": StatusView,
  "/print": Print,
  "/scan": Scan,
  "/settings": Settings,
  "/setup": Setup,
};

const nav: { to: Route; label: string; icon: IconName }[] = [
  { to: "/", label: strings.nav.status, icon: "status" },
  { to: "/print", label: strings.nav.print, icon: "print" },
  { to: "/scan", label: strings.nav.scan, icon: "scan" },
  { to: "/settings", label: strings.nav.settings, icon: "settings" },
];

/**
 * Overall health for the header dot, from the overall phase. Probing happens only before setup,
 * so "No device found" outranks "Not set up": nothing usable is plugged in for the wizard.
 */
export function overallTone(
  status: Status | null,
  connected: boolean,
  nothingFound = false,
): { tone: Tone; label: string; pulse: boolean } {
  if (!connected) return { tone: "idle", label: strings.connection.offline, pulse: false };
  if (nothingFound) return { tone: "problem", label: strings.connection.notFound, pulse: false };
  if (status?.config.state === "missing")
    return { tone: "idle", label: strings.connection.idle, pulse: false };
  switch (status?.phase) {
    case "ready":
      return { tone: "ok", label: strings.connection.ready, pulse: false };
    case "probing":
    case "deploying":
      return { tone: "busy", label: strings.connection.working, pulse: true };
    case "degraded":
    case "error":
      return { tone: "problem", label: strings.connection.problem, pulse: false };
    case "none":
      return { tone: "idle", label: strings.connection.nothingInUse, pulse: false };
    default:
      return { tone: "idle", label: strings.connection.idle, pulse: false };
  }
}

/** The configured name (the UI title), refreshed whenever the config file's ETag changes. */
function useConfigName(status: Status | null): string | null {
  const [name, setName] = useState<string | null>(null);
  const etag = status?.config.etag;
  const state = status?.config.state;
  // biome-ignore lint/correctness/useExhaustiveDependencies: a new etag means the file changed, so refetch
  useEffect(() => {
    if (!state || state === "missing") {
      setName(null);
      return;
    }
    let live = true;
    api.getConfig().then(
      (r) => {
        if (live) setName(r.config?.name ?? r.lastGood?.name ?? null);
      },
      () => {
        // The name is decoration; Status and Settings report config problems.
      },
    );
    return () => {
      live = false;
    };
  }, [etag, state]);
  return name;
}

/** True once the stream has been down for longer than a brief reconnect. */
function useOffline(connected: boolean, graceMs = 2500): boolean {
  const [offline, setOffline] = useState(false);
  useEffect(() => {
    if (connected) {
      setOffline(false);
      return;
    }
    const t = setTimeout(() => setOffline(true), graceMs);
    return () => clearTimeout(t);
  }, [connected, graceMs]);
  return offline;
}

function NotFound() {
  return (
    <header className="ui-page-header">
      <div className="ui-page-heading">
        <h1 tabIndex={-1}>{strings.notFound.title}</h1>
        <p className="ui-page-description">{strings.notFound.body}</p>
        <a href="/">{strings.notFound.home}</a>
      </div>
    </header>
  );
}

function Banners({
  status,
  offline,
  path,
}: {
  status: Status | null;
  offline: boolean;
  path: string;
}) {
  // Settings shows the file's errors itself.
  const invalid = status?.config.state === "invalid" && path !== "/settings";
  const firstError = invalid ? status.config.errors[0] : undefined;
  return (
    <>
      {offline && (
        <div className="app-banner" data-tone="busy" role="status">
          <Icon name="offline" size={18} className="app-banner-icon" />
          <p>{strings.banners.offline}</p>
        </div>
      )}
      {invalid && (
        <div className="app-banner" data-tone="problem" role="alert">
          <Icon name="alert" size={18} className="app-banner-icon" />
          <div className="app-banner-text">
            <p>{strings.banners.invalidConfig}</p>
            {firstError && (
              <p className="app-banner-detail">
                {strings.banners.invalidConfigLine(firstError.line, firstError.message)}
              </p>
            )}
          </div>
          <a className="app-banner-link" href="/settings">
            {strings.banners.openSettings}
          </a>
        </div>
      )}
    </>
  );
}

/** Print and Scan are hidden while no printer or scanner is enabled. */
function useHiddenRoutes(status: Status | null): Route[] {
  const printers = useEnabledPrinters();
  const scanners = useEnabledScanners();
  // Unknown until the first Status: hide nothing, so a reload on /print stays there.
  if (status === null || status.config.state === "missing") return [];
  return [
    ...(printers.length === 0 ? (["/print"] as const) : []),
    ...(scanners.length === 0 ? (["/scan"] as const) : []),
  ];
}

function Shell({ views }: { views: Views }) {
  const path = usePath();
  useLinkInterception();
  useFocusOnNavigate();
  const status = useStatus();
  const connected = useConnected();
  const offline = useOffline(connected);
  const name = useConfigName(status);
  const configMissing = status?.config.state === "missing";
  const hidden = useHiddenRoutes(status);
  const pathHidden = (hidden as string[]).includes(path);
  useDocumentTitle(name ?? strings.appName);

  // A problem belongs to the page it happened on. Keyed on the path: wizard steps change only the hash.
  // biome-ignore lint/correctness/useExhaustiveDependencies: path is the trigger
  useEffect(() => dismissProblemToasts(), [path]);

  useEffect(() => {
    if (configMissing && path !== "/setup") navigate("/setup", { replace: true, force: true });
    else if (pathHidden) navigate("/", { replace: true, force: true });
  }, [configMissing, path, pathHidden]);

  const View = pathHidden
    ? views["/"]
    : ((views as Record<string, (() => ReactNode) | undefined>)[path] ?? NotFound);
  const health = overallTone(status, connected, useNothingFound());
  const showNav = !configMissing;

  return (
    <div className="app" data-nav={showNav || undefined}>
      <a className="app-skip" href="#main">
        {strings.skipToContent}
      </a>
      <header className="app-header">
        <div className="app-identity">
          <svg className="app-mark" viewBox="0 0 24 24" aria-hidden="true" focusable="false">
            <rect x="5" y="3" width="14" height="18" rx="2" />
            <path d="M8.5 8h7M8.5 11.5h7M8.5 15h4" />
          </svg>
          <p className="app-name">{name ?? strings.appName}</p>
        </div>
        <StatusBadge tone={health.tone} pulse={health.pulse} animate={connected && status !== null}>
          {health.label}
        </StatusBadge>
      </header>
      {showNav && (
        <nav className="app-nav" aria-label={strings.nav.label}>
          <ul>
            {nav
              .filter((item) => !hidden.includes(item.to))
              .map((item) => (
                <li key={item.to}>
                  <a
                    href={item.to}
                    className="app-nav-link"
                    aria-current={path === item.to ? "page" : undefined}
                  >
                    <Icon name={item.icon} size={22} />
                    <span>{item.label}</span>
                  </a>
                </li>
              ))}
          </ul>
        </nav>
      )}
      <main id="main" className="app-main" tabIndex={-1}>
        <Banners status={status} offline={offline} path={path} />
        <View />
      </main>
      <ToastRegion />
    </div>
  );
}

export function App({
  store = defaultStore,
  views = defaultViews,
}: {
  store?: EventStore;
  /** The page per route; tests pass stand-ins to check the shell alone. */
  views?: Views;
}) {
  useEffect(() => {
    store.start();
    return () => store.stop();
  }, [store]);
  return (
    <EventsContext value={store}>
      <RouterProvider navigate={(to) => navigate(String(to))}>
        <Shell views={views} />
      </RouterProvider>
    </EventsContext>
  );
}
