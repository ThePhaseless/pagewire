// The Scan page: settings limited to what the scanner offers, a Scan button, live pages as
// thumbnails with a full-size preview, the download, and the scan history with delete.
import { useContext, useEffect, useRef, useState } from "react";
import { Button as AriaButton } from "react-aria-components";
import { api, ProblemError, problemMessage } from "../api/client";
import { EventsContext, useConnected, usePrinterNotFound, useStatus } from "../api/events";
import {
  Button,
  Card,
  Dialog,
  EmptyState,
  Icon,
  InlineError,
  LinkButton,
  PageHeader,
  ProgressBar,
  RadioGroup,
  Select,
  StatusBadge,
  type Tone,
  toast,
} from "../components/ui";
import type { Config, Scan as ScanDTO, ScannerCaps, ScanSettings } from "../gen/api";
import { strings } from "../strings";
import { scanStrings as t } from "./Scan.strings";
import "./Scan.css";

// Mirrors config.Default: what the server applies when the file leaves scan defaults out.
const fallback: ScanSettings = {
  source: "platen",
  resolution: 300,
  colorMode: "color",
  format: "pdf",
};

const sources = ["platen", "adf", "adf-duplex"] as const;
const colorModes = ["color", "grayscale", "blackandwhite"] as const;
const isOneOf = <T extends string>(list: readonly T[], v: string): v is T =>
  (list as readonly string[]).includes(v);

/** What the scanner offers, limited to the values the API accepts. */
function offered(caps: ScannerCaps) {
  return {
    sources: caps.sources.filter((s) => isOneOf(sources, s)),
    resolutions: caps.resolutions.filter((r) => r >= 75 && r <= 1200),
    colorModes: caps.colorModes.filter((c) => isOneOf(colorModes, c)),
  };
}

/**
 * `want` limited to `caps`: each wanted value the scanner offers is kept; otherwise the nearest
 * resolution, or the first offered source and colour mode. A field the scanner offers nothing
 * for keeps the wanted value (the Scan button is disabled then).
 */
function fitSettings(want: ScanSettings, caps: ScannerCaps): ScanSettings {
  const o = offered(caps);
  const nearest = o.resolutions.reduce<number | undefined>(
    (best, r) =>
      best === undefined || Math.abs(r - want.resolution) < Math.abs(best - want.resolution)
        ? r
        : best,
    undefined,
  );
  return {
    source: o.sources.includes(want.source) ? want.source : (o.sources[0] ?? want.source),
    resolution: nearest ?? want.resolution,
    colorMode: o.colorModes.includes(want.colorMode)
      ? want.colorMode
      : (o.colorModes[0] ?? want.colorMode),
    format: want.format,
  };
}

/** Newest first, one entry per id. */
function mergeScans(current: ScanDTO[], incoming: ScanDTO[]): ScanDTO[] {
  const byId = new Map(current.map((s) => [s.id, s]));
  for (const s of incoming) byId.set(s.id, s);
  return [...byId.values()].sort(
    (a, b) => Date.parse(b.createdAt) - Date.parse(a.createdAt) || b.id.localeCompare(a.id),
  );
}

function downloadLabel(scan: ScanDTO): string {
  if (scan.format === "pdf") return t.download.pdf;
  return scan.pages === 1 ? t.download.jpg : t.download.zip;
}

const stateTone: Record<ScanDTO["state"], Tone> = {
  scanning: "busy",
  completed: "ok",
  failed: "problem",
  canceled: "idle",
};

const dateFormat = new Intl.DateTimeFormat(undefined, { dateStyle: "medium", timeStyle: "medium" });
const formatTime = (iso: string) => dateFormat.format(Date.parse(iso));

/** The effective config (the file's, else its last good version), refreshed on every file change. */
function useConfig(etag: string | undefined, state: string | undefined) {
  const [config, setConfig] = useState<Config | null>(null);
  // biome-ignore lint/correctness/useExhaustiveDependencies: a new etag means the file changed, so refetch
  useEffect(() => {
    if (!state || state === "missing") return;
    let live = true;
    api.getConfig().then(
      (r) => {
        if (live) setConfig(r.config ?? r.lastGood);
      },
      () => {
        // The fallback defaults still work; Status and Settings report config problems.
      },
    );
    return () => {
      live = false;
    };
  }, [etag, state]);
  return config;
}

/** The scanner's capabilities, asked only while it is available (GET /scanner answers 503 otherwise). */
function useCaps(available: boolean) {
  const store = useContext(EventsContext);
  const [caps, setCaps] = useState<ScannerCaps | null>(null);
  const [error, setError] = useState<unknown>(null);
  // Bumped to ask again: by the retry button, and after the event stream reconnects.
  const [attempt, setAttempt] = useState(0);
  const retry = () => {
    setError(null);
    setAttempt((n) => n + 1);
  };
  useEffect(() => store.onReconnect(() => setAttempt((n) => n + 1)), [store]);
  // biome-ignore lint/correctness/useExhaustiveDependencies: attempt is intentionally a refetch trigger
  useEffect(() => {
    if (!available) return;
    let live = true;
    api.getScanner().then(
      (c) => {
        if (!live) return;
        setCaps(c);
        setError(null);
      },
      (err) => {
        if (live) setError(err);
      },
    );
    return () => {
      live = false;
    };
  }, [available, attempt]);
  return { caps: available ? caps : null, error: available ? error : null, retry };
}

/**
 * GET /scans once, after every SSE reconnect and on retry, with `scan` events merged by id. A scan
 * this page saw running that ends completed or failed is announced in a toast.
 */
function useScans() {
  const store = useContext(EventsContext);
  const [scans, setScans] = useState<ScanDTO[]>([]);
  const [error, setError] = useState<unknown>(null);
  const [attempt, setAttempt] = useState(0);
  // The last state seen per scan id, to tell a scan finishing from one loaded finished.
  const seen = useRef(new Map<string, ScanDTO["state"]>());

  // biome-ignore lint/correctness/useExhaustiveDependencies: attempt is a refetch trigger
  useEffect(() => {
    let live = true;
    const load = () =>
      api.getScans().then(
        (list) => {
          if (!live) return;
          for (const s of list) seen.current.set(s.id, s.state);
          setScans(mergeScans([], list));
          setError(null);
        },
        (err) => {
          if (live) setError(err);
        },
      );
    void load();
    const offReconnect = store.onReconnect(() => void load());
    const offScan = store.on("scan", (s) => {
      const before = seen.current.get(s.id);
      seen.current.set(s.id, s.state);
      if (before === "scanning" && s.state === "completed")
        toast({ title: t.ready(s.pages), tone: "ok" });
      if (before === "scanning" && s.state === "failed")
        toast({ title: t.failed, description: s.error ?? undefined, tone: "problem" });
      setScans((prev) => mergeScans(prev, [s]));
    });
    return () => {
      live = false;
      offReconnect();
      offScan();
    };
  }, [store, attempt]);

  /** Adds or updates scans this page got from its own requests. */
  const add = (list: ScanDTO[]) => {
    for (const s of list) if (!seen.current.has(s.id)) seen.current.set(s.id, s.state);
    // A scan event may already have brought a newer state of the same scan.
    setScans((prev) =>
      mergeScans(
        prev,
        list.filter((s) => !prev.some((p) => p.id === s.id)),
      ),
    );
  };
  const retry = () => {
    setError(null);
    setAttempt((n) => n + 1);
  };
  return { scans, setScans, add, error, retry };
}

function Thumbnails({ scan }: { scan: ScanDTO }) {
  const [preview, setPreview] = useState<number | null>(null);
  if (scan.pages === 0) return null;
  const pages = Array.from({ length: scan.pages }, (_, i) => i + 1);
  return (
    <>
      <ul className="scan-thumbs">
        {pages.map((n) => (
          <li key={n}>
            <AriaButton
              className="scan-thumb"
              aria-label={t.result.openPage(n)}
              onPress={() => setPreview(n)}
            >
              <img src={api.scanPageUrl(scan.id, n)} alt={t.result.page(n)} loading="lazy" />
              <span className="scan-thumb-label tabular" aria-hidden="true">
                {n}
              </span>
            </AriaButton>
          </li>
        ))}
      </ul>
      <Dialog
        size="lg"
        isOpen={preview !== null}
        onOpenChange={(open) => !open && setPreview(null)}
        title={preview === null ? "" : t.result.pageOf(preview, scan.pages)}
      >
        {preview !== null && (
          <>
            <img
              className="scan-preview"
              src={api.scanPageUrl(scan.id, preview)}
              alt={t.result.page(preview)}
            />
            <div className="scan-dialog-actions">
              <Button isDisabled={preview <= 1} onPress={() => setPreview(preview - 1)}>
                {t.result.previous}
              </Button>
              <Button isDisabled={preview >= scan.pages} onPress={() => setPreview(preview + 1)}>
                {t.result.next}
              </Button>
            </div>
          </>
        )}
      </Dialog>
    </>
  );
}

function Result({ scan, onCancel }: { scan: ScanDTO | undefined; onCancel: (id: string) => void }) {
  if (!scan) {
    return (
      <Card title={t.result.title} className="scan-result">
        <EmptyState icon="scan" title={t.result.empty}>
          {t.result.emptyBody}
        </EmptyState>
      </Card>
    );
  }
  return (
    <Card
      title={t.result.title}
      className="scan-result"
      actions={
        scan.state === "completed" ? (
          <LinkButton variant="secondary" href={api.scanDownloadUrl(scan.id)} download>
            <Icon name="download" size={18} />
            {downloadLabel(scan)}
          </LinkButton>
        ) : (
          scan.state === "scanning" && <Button onPress={() => onCancel(scan.id)}>{t.cancel}</Button>
        )
      }
    >
      <p className="scan-subtle">
        {scan.state === "scanning"
          ? formatTime(scan.createdAt)
          : t.result.meta(
              formatTime(scan.createdAt),
              t.history.summary(scan.pages, t.options.format[scan.format]),
            )}
      </p>
      {scan.state === "scanning" && <ProgressBar label={t.scanning(scan.pages)} />}
      {scan.state === "failed" && (
        <InlineError>
          <strong>{t.result.failed}</strong> {scan.error}
        </InlineError>
      )}
      {scan.state === "canceled" && <p className="scan-subtle">{t.result.canceled}</p>}
      <Thumbnails key={scan.id} scan={scan} />
    </Card>
  );
}

export function Scan() {
  const status = useStatus();
  const connected = useConnected();
  const notFound = usePrinterNotFound();
  const available = status?.scanner.available ?? false;
  const config = useConfig(status?.config.etag, status?.config.state);
  const { caps, error: capsError, retry: retryCaps } = useCaps(available);
  const { scans, setScans, add, error: listError, retry: retryList } = useScans();

  const [overrides, setOverrides] = useState<Partial<ScanSettings>>({});
  const want: ScanSettings = { ...fallback, ...config?.scan.defaults, ...overrides };
  const settings = caps ? fitSettings(want, caps) : want;
  const o = caps ? offered(caps) : null;
  const set = (patch: Partial<ScanSettings>) => setOverrides((v) => ({ ...v, ...patch }));

  const [starting, setStarting] = useState(false);
  const [startError, setStartError] = useState<string | null>(null);
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [confirmId, setConfirmId] = useState<string | null>(null);

  const busy =
    starting || status?.scanner.state === "processing" || scans.some((s) => s.state === "scanning");
  // Not `busy`: the button shows pending instead, so keyboard focus stays on it.
  const canScan =
    available &&
    connected &&
    o !== null &&
    o.sources.length > 0 &&
    o.resolutions.length > 0 &&
    o.colorModes.length > 0;
  const shown = scans.find((s) => s.id === selectedId) ?? scans[0];

  async function start() {
    setStarting(true);
    setStartError(null);
    try {
      const s = await api.startScan(settings);
      add([s]);
      setSelectedId(s.id);
    } catch (err) {
      setStartError(
        err instanceof ProblemError && err.status === 409 ? t.busy : problemMessage(err),
      );
    } finally {
      setStarting(false);
    }
  }

  async function remove(id: string) {
    try {
      const running = await api.deleteScan(id);
      // A scan that was still running is canceled instead, and stays listed.
      if (running) add([running]);
      else {
        setScans((prev) => prev.filter((s) => s.id !== id));
        toast({ title: t.confirmDelete.deleted, tone: "ok" });
      }
    } catch (err) {
      toast({ title: t.confirmDelete.failed, description: problemMessage(err), tone: "problem" });
    }
  }

  async function cancel(id: string) {
    try {
      // 202: the `scan` event brings the canceled state.
      await api.deleteScan(id);
    } catch (err) {
      toast({ title: t.cancelFailed, description: problemMessage(err), tone: "problem" });
    }
  }

  return (
    <div className="scan-view">
      <PageHeader title={strings.nav.scan} description={t.description} />

      {!available && (
        <Card className="scan-unavailable">
          {status === null ? (
            <p className="scan-subtle">{t.unavailable.loading}</p>
          ) : (
            <EmptyState
              icon="scan"
              title={t.unavailable.title}
              action={
                <LinkButton variant="secondary" href="/">
                  {t.unavailable.seeStatus}
                </LinkButton>
              }
            >
              <p>{status.device.message || t.unavailable.waiting}</p>
              {notFound && <p>{strings.plugInHint}</p>}
            </EmptyState>
          )}
        </Card>
      )}

      <div className="scan-grid">
        <Card title={t.options.title} className="scan-options">
          {available && capsError ? (
            <InlineError action={<Button onPress={retryCaps}>{strings.buttons.retry}</Button>}>
              {problemMessage(capsError)}
            </InlineError>
          ) : null}
          {available && !caps && !capsError && <p className="scan-subtle">{t.options.loading}</p>}
          {o && (
            <>
              <Select
                label={t.options.source.label}
                selectedKey={settings.source}
                onSelectionChange={(k) => set({ source: k as ScanSettings["source"] })}
                options={o.sources.map((id) => ({ id, label: t.options.source[id] }))}
              />
              <Select
                label={t.options.resolution}
                selectedKey={String(settings.resolution)}
                onSelectionChange={(k) => set({ resolution: Number(k) })}
                options={o.resolutions.map((r) => ({ id: String(r), label: t.options.dpi(r) }))}
              />
              <Select
                label={t.options.colorMode.label}
                selectedKey={settings.colorMode}
                onSelectionChange={(k) => set({ colorMode: k as ScanSettings["colorMode"] })}
                options={o.colorModes.map((id) => ({ id, label: t.options.colorMode[id] }))}
              />
            </>
          )}
          <RadioGroup
            label={t.options.format.label}
            value={settings.format}
            onChange={(v) => set({ format: v as ScanSettings["format"] })}
            isDisabled={!available}
            options={[
              { id: "pdf", label: t.options.format.pdf, description: t.options.format.pdfHint },
              { id: "jpeg", label: t.options.format.jpeg, description: t.options.format.jpegHint },
            ]}
          />
          {startError && <InlineError>{startError}</InlineError>}
          <div className="scan-submit">
            <Button
              variant="primary"
              isDisabled={!canScan && !busy}
              isPending={busy}
              onPress={() => void start()}
            >
              <Icon name="scan" size={18} />
              {t.submit}
            </Button>
          </div>
        </Card>

        <Result scan={shown} onCancel={(id) => void cancel(id)} />
      </div>

      <Card title={t.history.title} className="scan-history">
        {config && <p className="scan-subtle">{t.history.retention(config.scan.retentionDays)}</p>}
        {listError ? (
          <InlineError action={<Button onPress={retryList}>{strings.buttons.retry}</Button>}>
            {t.history.loadFailed}
          </InlineError>
        ) : (
          scans.length === 0 && (
            <EmptyState icon="scan" title={t.history.empty}>
              {t.history.emptyBody}
            </EmptyState>
          )
        )}
        {scans.length > 0 && (
          <ul className="scan-list" aria-label={t.history.title}>
            {scans.map((s) => {
              const when = formatTime(s.createdAt);
              return (
                <li key={s.id} className="scan-row" data-current={s === shown || undefined}>
                  <span className="scan-row-main">
                    <span className="tabular">{when}</span>
                    <span className="scan-subtle">
                      {s.state === "scanning"
                        ? t.scanning(s.pages)
                        : t.history.summary(s.pages, t.options.format[s.format])}
                    </span>
                  </span>
                  <StatusBadge tone={stateTone[s.state]} pulse={s.state === "scanning"}>
                    {t.history.state[s.state]}
                  </StatusBadge>
                  <Button
                    variant="quiet"
                    aria-label={t.history.showScan(when)}
                    onPress={() => setSelectedId(s.id)}
                  >
                    {t.history.show}
                  </Button>
                  {s.state !== "scanning" && (
                    <Button
                      variant="quiet"
                      aria-label={t.history.deleteScan(when)}
                      onPress={() => setConfirmId(s.id)}
                    >
                      {strings.buttons.delete}
                    </Button>
                  )}
                </li>
              );
            })}
          </ul>
        )}
      </Card>

      <Dialog
        role="alertdialog"
        isOpen={confirmId !== null}
        onOpenChange={(open) => !open && setConfirmId(null)}
        title={t.confirmDelete.title}
      >
        {(close) => (
          <>
            <p>{t.confirmDelete.body}</p>
            <div className="scan-dialog-actions">
              <Button onPress={close}>{strings.buttons.cancel}</Button>
              <Button
                variant="danger"
                onPress={() => {
                  if (confirmId) void remove(confirmId);
                  close();
                }}
              >
                {t.confirmDelete.confirm}
              </Button>
            </div>
          </>
        )}
      </Dialog>
    </div>
  );
}
