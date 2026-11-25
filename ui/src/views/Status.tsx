// The Status dashboard: one card per enabled printer and scanner, with recent jobs, recent scans
// and the system state beside them.
import { useContext, useEffect, useState } from "react";
import { api, problemMessage } from "../api/client";
import { EventsContext, useJobs, useStatus } from "../api/events";
import {
  Button,
  Card,
  DeviceStatusBadge,
  Disclosure,
  EmptyState,
  Icon,
  InlineError,
  LinkButton,
  Meter,
  PageHeader,
  StatusBadge,
  Tab,
  TabList,
  TabPanel,
  Tabs,
  type Tone,
  toast,
} from "../components/ui";
import type {
  Job,
  PrinterConnect,
  Scan,
  ScannerConnect,
  Status as StatusDTO,
  StatusPrinter,
  StatusScanner,
  StatusWorkload,
  USBDevice,
  Warning,
} from "../gen/api";
import { enabled } from "../lib/devices";
import { type DeviceRole, warningText } from "../lib/labels";
import { strings } from "../strings";
import { statusStrings as t } from "./Status.strings";
import "./Status.css";

const RECENT = 5;

const endpointTone: Record<StatusPrinter["state"], Tone> = {
  idle: "ok",
  processing: "busy",
  stopped: "problem",
  unknown: "idle",
};

const jobTone: Record<Job["state"], Tone> = {
  pending: "busy",
  processing: "active",
  completed: "ok",
  canceled: "idle",
  aborted: "problem",
  held: "idle",
  stopped: "problem",
};

const scanTone: Record<Scan["state"], Tone> = {
  scanning: "active",
  completed: "ok",
  failed: "problem",
  canceled: "idle",
};

const blobTone = { installed: "ok", missing: "idle", failed: "problem", disabled: "idle" } as const;

const dateFormat = new Intl.DateTimeFormat(undefined, { dateStyle: "medium", timeStyle: "short" });

/** A formatted local time, or null for the zero time the server sends before anything happened. */
function formatTime(iso: string): string | null {
  const ms = Date.parse(iso);
  return ms > 0 ? dateFormat.format(ms) : null;
}

/** IPP printer-state-reasons keyword to plain words; "none" yields null. */
export function reasonText(keyword: string): string | null {
  const base = keyword.replace(/-(error|warning|report)$/, "");
  if (base === "none") return null;
  const known = t.printer.reasons[base];
  if (known) return known;
  const words = base.replace(/-/g, " ");
  return words.charAt(0).toUpperCase() + words.slice(1);
}

// Marker colours come from the printer; only plain #rrggbb values reach the style prop.
const markerColor = (c: string) => (/^#[0-9a-f]{6}$/i.test(c) ? c : undefined);

const family = (id: string) => strings.device.families[id] ?? id;

/**
 * The node a printer or scanner runs on, and that node's workload. Simulated entries have no
 * detected USB device, so they get the only workload when there is exactly one.
 */
function placeOf(status: StatusDTO, role: DeviceRole, id: string) {
  const found = status.devices.find((d) => (role === "printer" ? d.printerId : d.scannerId) === id);
  const workload = found
    ? status.workloads.find((w) => w.node === found.node)
    : status.workloads.length === 1
      ? status.workloads[0]
      : undefined;
  return { node: found?.node ?? "", workload };
}

/** The card heading: kind icon and the entry's name. */
function DeviceTitle({ kind, name }: { kind: DeviceRole; name: string }) {
  return (
    <span className="status-device-title">
      <Icon name={kind === "printer" ? "print" : "scan"} size={22} />
      <span>{name}</span>
    </span>
  );
}

/** The phase, or once the device answers, what it is doing (Ready, Printing, Stopped). */
function DeviceBadge({
  device,
  kind,
}: {
  device: StatusPrinter | StatusScanner;
  kind: DeviceRole;
}) {
  if (device.phase !== "ready" || !device.available)
    return <DeviceStatusBadge phase={device.phase} />;
  const words = kind === "printer" ? t.printer.state : t.scanner.state;
  return <StatusBadge tone={endpointTone[device.state]}>{words[device.state]}</StatusBadge>;
}

/** Location, the phase message (an alert in phase error) and the option warnings. */
function DeviceIntro({
  device,
  kind,
}: {
  device: StatusPrinter | StatusScanner;
  kind: DeviceRole;
}) {
  return (
    <>
      {device.location && <p className="status-subtle">{device.location}</p>}
      {device.message &&
        (device.phase === "error" ? (
          <InlineError>{device.message}</InlineError>
        ) : (
          <p className="status-message">{device.message}</p>
        ))}
      <Warnings warnings={device.warnings} kind={kind} />
    </>
  );
}

function Warnings({ warnings, kind }: { warnings: Warning[]; kind: DeviceRole }) {
  if (warnings.length === 0) return null;
  return (
    <section className="status-section">
      <h3>{t.device.warningsLabel}</h3>
      <ul className="status-list">
        {warnings.map((w) => (
          <li key={`${w.option}:${w.value}:${w.message}`}>{warningText(w, kind)}</li>
        ))}
      </ul>
    </section>
  );
}

function usbName(usb: USBDevice) {
  return `${usb.manufacturer} ${usb.product}`.trim() || t.device.unnamedDevice;
}

/** The shared head of Technical details: which device, its USB IDs and its node. */
function DeviceFacts({ usb, node }: { usb: USBDevice | null | undefined; node: string }) {
  return (
    <>
      <div>
        <dt>{t.device.deviceLabel}</dt>
        <dd>{usb ? usbName(usb) : t.device.simulatedDevice}</dd>
      </div>
      {usb && (
        <div>
          <dt>{t.device.usbLabel}</dt>
          <dd>
            {[
              t.device.usbIds(usb.vendorId, usb.productId),
              usb.serial && t.device.serial(usb.serial),
            ]
              .filter(Boolean)
              .join(", ")}
          </dd>
        </div>
      )}
      {node && (
        <div>
          <dt>{t.device.nodeLabel}</dt>
          <dd>{node}</dd>
        </div>
      )}
    </>
  );
}

/** This device's own DNS-SD services among those its workload advertises. */
function Advertised({ workload, name }: { workload?: StatusWorkload; name: string }) {
  const services = name ? (workload?.advertised ?? []).filter((s) => s.name === name) : [];
  return (
    <section className="status-section">
      <h3>{t.advertised.title}</h3>
      {services.length === 0 ? (
        <p className="status-subtle">{t.advertised.notDiscoverable}</p>
      ) : (
        <>
          <p className="status-check">
            <Icon name="check" size={18} />
            {t.advertised.discoverable}
          </p>
          <ul className="status-list">
            {services.map((s, i) => (
              // Avahi can list one service once per interface, so entries repeat.
              // biome-ignore lint/suspicious/noArrayIndexKey: entries have no unique id
              <li key={i}>
                <span>{`${t.advertised.types[s.type] ?? s.type}: ${s.name}`}</span>
                <span className="status-subtle tabular">{t.advertised.port(s.port)}</span>
              </li>
            ))}
          </ul>
        </>
      )}
    </section>
  );
}

function PrinterCard({ printer, status }: { printer: StatusPrinter; status: StatusDTO }) {
  const { node, workload } = placeOf(status, "printer", printer.id);
  const d = printer.decision;
  const usb = printer.device ?? d?.device;
  const reasons = [
    ...new Set(printer.reasons.map(reasonText).filter((r): r is string => r !== null)),
  ];
  // shortcut: a vendor file is matched to the printer by its EULA id (true of every catalog blob);
  // match on the blob's own EULA field once BlobStatus carries it.
  const eulas = d ? [...d.requiredEulas, ...d.optionalEulas] : [];
  const blobs = (workload?.blobs ?? []).filter((b) => eulas.includes(b.id));
  return (
    <Card
      className="status-device"
      title={<DeviceTitle kind="printer" name={printer.name} />}
      actions={<DeviceBadge device={printer} kind="printer" />}
    >
      <DeviceIntro device={printer} kind="printer" />
      {d?.rule === "none" && (
        <section className="status-section">
          <h3>{strings.device.noDriver.title}</h3>
          <p className="status-message">
            {usb?.deviceId ? (
              <>
                {strings.device.noDriver.deviceId} <code>{usb.deviceId}</code>
              </>
            ) : (
              strings.device.noDriver.noDeviceId
            )}
          </p>
          <div>
            <LinkButton href="/settings">{t.device.chooseDriver}</LinkButton>
          </div>
        </section>
      )}
      {reasons.length > 0 && (
        <section className="status-section">
          <h3>{t.printer.reasonsLabel}</h3>
          <ul className="status-list">
            {reasons.map((r) => (
              <li key={r}>{r}</li>
            ))}
          </ul>
        </section>
      )}
      {printer.available ? (
        <div>
          <LinkButton href={`/print?printer=${encodeURIComponent(printer.id)}`}>
            {t.printer.open}
          </LinkButton>
        </div>
      ) : (
        <p className="status-subtle">{t.printer.unavailable}</p>
      )}
      {printer.markers.length > 0 && (
        <section className="status-section">
          <h3>{t.printer.suppliesLabel}</h3>
          {printer.markers.map((m, i) =>
            m.level >= 0 ? (
              <Meter
                // biome-ignore lint/suspicious/noArrayIndexKey: marker names may repeat or be empty
                key={i}
                label={m.name || t.printer.unnamedSupply}
                value={Math.min(m.level, 100)}
                color={markerColor(m.color)}
              />
            ) : (
              // biome-ignore lint/suspicious/noArrayIndexKey: marker names may repeat or be empty
              <p key={i} className="status-row">
                <span>{m.name || t.printer.unnamedSupply}</span>
                <span className="status-subtle">{t.printer.levelUnknown}</span>
              </p>
            ),
          )}
        </section>
      )}
      <Connect kind="printer" connect={printer.connect} />
      <Disclosure title={strings.technicalDetails}>
        <dl className="status-facts">
          <DeviceFacts usb={usb} node={node} />
          {d && (
            <>
              <div>
                <dt>{t.device.ruleLabel}</dt>
                <dd>{strings.device.rules[d.rule]}</dd>
              </div>
              <div>
                <dt>{t.device.printerFamilyLabel}</dt>
                <dd>{d.family ? family(d.family) : t.device.noFamily}</dd>
              </div>
              <div>
                <dt>{t.device.driverLabel}</dt>
                <dd>
                  {d.driverDescription ||
                    d.driver ||
                    // A PAPPL family without a driver name runs its own automatic choice.
                    (d.family && d.family !== "ipp-usb" && d.family !== "sim"
                      ? t.device.appDriver
                      : t.device.noFamily)}
                </dd>
              </div>
            </>
          )}
        </dl>
        <Advertised workload={workload} name={printer.connect.dnssdName} />
        {blobs.length > 0 && (
          <section className="status-section">
            <h3>{t.device.blobsTitle}</h3>
            <ul className="status-list">
              {blobs.map((b) => (
                <li key={b.id}>
                  <span className="status-row">
                    <span>{`${b.id} ${b.version}`}</span>
                    <StatusBadge tone={blobTone[b.state]}>
                      {t.device.blobState[b.state]}
                    </StatusBadge>
                  </span>
                  {b.message && <span className="status-subtle">{b.message}</span>}
                </li>
              ))}
            </ul>
          </section>
        )}
      </Disclosure>
    </Card>
  );
}

function ScannerCard({ scanner, status }: { scanner: StatusScanner; status: StatusDTO }) {
  const { node, workload } = placeOf(status, "scanner", scanner.id);
  const d = scanner.decision;
  return (
    <Card
      className="status-device"
      title={<DeviceTitle kind="scanner" name={scanner.name} />}
      actions={<DeviceBadge device={scanner} kind="scanner" />}
    >
      <DeviceIntro device={scanner} kind="scanner" />
      {scanner.available ? (
        <div>
          <LinkButton href={`/scan?scanner=${encodeURIComponent(scanner.id)}`}>
            {t.scanner.open}
          </LinkButton>
        </div>
      ) : (
        <p className="status-subtle">{t.scanner.unavailable}</p>
      )}
      <Connect kind="scanner" connect={scanner.connect} />
      <Disclosure title={strings.technicalDetails}>
        <dl className="status-facts">
          <DeviceFacts usb={scanner.device ?? d?.device} node={node} />
          {d && (
            <>
              <div>
                <dt>{t.device.ruleLabel}</dt>
                <dd>{strings.device.scannerRules[d.rule]}</dd>
              </div>
              <div>
                <dt>{t.device.scannerFamilyLabel}</dt>
                <dd>{d.family ? family(d.family) : t.device.noFamily}</dd>
              </div>
              {d.saneBackends.length > 0 && (
                <div>
                  <dt>{t.device.saneBackendsLabel}</dt>
                  <dd>{d.saneBackends.join(", ")}</dd>
                </div>
              )}
            </>
          )}
        </dl>
        <Advertised workload={workload} name={scanner.connect.dnssdName} />
      </Disclosure>
    </Card>
  );
}

function CopyButton({ value }: { value: string }) {
  const [copied, setCopied] = useState(false);
  useEffect(() => {
    if (!copied) return;
    const timer = setTimeout(() => setCopied(false), 2000);
    return () => clearTimeout(timer);
  }, [copied]);
  // navigator.clipboard only exists in secure contexts; over plain HTTP the address stays selectable.
  if (typeof navigator === "undefined" || !navigator.clipboard) return null;
  return (
    <Button
      variant="quiet"
      aria-label={t.connect.copyLabel(value)}
      onPress={() => {
        navigator.clipboard.writeText(value).then(
          () => setCopied(true),
          () => toast({ title: t.connect.copyFailed, tone: "problem" }),
        );
      }}
    >
      <Icon name={copied ? "check" : "copy"} size={18} />
      {copied ? strings.buttons.copied : strings.buttons.copy}
    </Button>
  );
}

function Addresses({ label, values }: { label: string; values: string[] }) {
  return (
    <div>
      <dt>{label}</dt>
      {values.length === 0 ? (
        <dd className="status-subtle">{t.connect.noAddress}</dd>
      ) : (
        values.map((v) => (
          <dd key={v} className="status-address">
            <code>{v}</code>
            <CopyButton value={v} />
          </dd>
        ))
      )}
    </div>
  );
}

const osKeys = ["windows", "apple", "android", "linux"] as const;

type ConnectProps =
  | { kind: "printer"; connect: PrinterConnect }
  | { kind: "scanner"; connect: ScannerConnect };

/** How to reach this one device from a phone or computer: its steps and its own addresses. */
function Connect(props: ConnectProps) {
  const { connect } = props;
  const printer = props.kind === "printer";
  const name = connect.dnssdName || (printer ? t.connect.thisPrinter : t.connect.thisScanner);
  const steps = printer ? t.connect.printerSteps : t.connect.scannerSteps;
  return (
    <Disclosure title={t.connect.title}>
      <p className="status-subtle">{printer ? t.connect.printerIntro : t.connect.scannerIntro}</p>
      <dl className="status-facts">
        <div>
          <dt>{t.connect.nameLabel}</dt>
          <dd>{connect.dnssdName || t.connect.noName}</dd>
        </div>
      </dl>
      <Tabs>
        <TabList aria-label={t.connect.osLabel}>
          {osKeys.map((k) => (
            <Tab key={k} id={k}>
              {t.connect.os[k]}
            </Tab>
          ))}
        </TabList>
        {osKeys.map((k) => (
          <TabPanel key={k} id={k}>
            <ol className="status-steps">
              {steps[k](name).map((step) => (
                <li key={step}>{step}</li>
              ))}
            </ol>
          </TabPanel>
        ))}
      </Tabs>
      <section className="status-section">
        <h3>{t.connect.manualTitle}</h3>
        <dl className="status-addresses">
          {props.kind === "printer" ? (
            <Addresses label={t.connect.printerAddress} values={props.connect.ippUris} />
          ) : (
            <Addresses label={t.connect.scannerAddress} values={props.connect.esclUrls} />
          )}
        </dl>
        <p className="status-subtle">{t.connect.tailscale}</p>
      </section>
    </Disclosure>
  );
}

/** One printer's latest jobs; `named` adds the printer's name for a card listing several. */
function PrinterJobs({ printer, named }: { printer: StatusPrinter; named: boolean }) {
  const { jobs, error } = useJobs(printer.id);
  const recent = jobs.slice(0, RECENT);
  const body = (
    <>
      {printer.available && error ? <InlineError>{problemMessage(error)}</InlineError> : null}
      {recent.length > 0 ? (
        <ul className="status-list">
          {recent.map((j) => (
            <li key={j.id}>
              <span className="status-row">
                <span className="status-job-name">{j.name}</span>
                <StatusBadge tone={jobTone[j.state]} pulse={j.state === "processing"}>
                  {t.jobs.state[j.state]}
                </StatusBadge>
              </span>
              <span className="status-subtle tabular">{formatTime(j.createdAt)}</span>
            </li>
          ))}
        </ul>
      ) : named ? (
        <p className="status-subtle">{t.jobs.empty}</p>
      ) : (
        <EmptyState icon="print" title={t.jobs.empty}>
          {printer.available ? t.jobs.emptyBody : t.jobs.unavailable}
        </EmptyState>
      )}
      {jobs.length > 0 && (
        <div>
          <LinkButton
            variant="quiet"
            href={`/print?printer=${encodeURIComponent(printer.id)}`}
            aria-label={named ? t.jobs.seeAllOf(printer.name) : undefined}
          >
            {t.jobs.seeAll}
          </LinkButton>
        </div>
      )}
    </>
  );
  if (!named) return body;
  return (
    <section className="status-section">
      <h3>{printer.name}</h3>
      {body}
    </section>
  );
}

function JobsCard({ printers }: { printers: StatusPrinter[] }) {
  return (
    <Card title={t.jobs.title}>
      {printers.map((p) => (
        <PrinterJobs key={p.id} printer={p} named={printers.length > 1} />
      ))}
    </Card>
  );
}

const newestScans = (list: Scan[]) =>
  [...list].sort((a, b) => Date.parse(b.createdAt) - Date.parse(a.createdAt)).slice(0, RECENT);

/** The latest scans: GET /scans, then `scan` events by id, reloaded on SSE reconnect. */
function useRecentScans() {
  const store = useContext(EventsContext);
  const [scans, setScans] = useState<Scan[]>([]);
  const [error, setError] = useState<unknown>(null);
  useEffect(() => {
    let live = true;
    const load = () =>
      api.getScans().then(
        (list) => {
          if (!live) return;
          setScans(newestScans(list));
          setError(null);
        },
        (err) => {
          if (live) setError(err);
        },
      );
    void load();
    const offReconnect = store.onReconnect(() => void load());
    const offScan = store.on("scan", (s) =>
      setScans((prev) => newestScans([s, ...prev.filter((p) => p.id !== s.id)])),
    );
    return () => {
      live = false;
      offReconnect();
      offScan();
    };
  }, [store]);
  return { scans, error };
}

function ScansCard({ scanners }: { scanners: StatusScanner[] }) {
  const { scans, error } = useRecentScans();
  const nameOf = (id: string) => scanners.find((s) => s.id === id)?.name ?? id;
  return (
    <Card title={t.scans.title}>
      {error ? <InlineError>{problemMessage(error)}</InlineError> : null}
      {scans.length > 0 ? (
        <>
          <ul className="status-list">
            {scans.map((s) => (
              <li key={s.id}>
                <span className="status-row">
                  <span className="status-job-name">{nameOf(s.scanner)}</span>
                  <StatusBadge tone={scanTone[s.state]} pulse={s.state === "scanning"}>
                    {t.scans.state[s.state]}
                  </StatusBadge>
                </span>
                <span className="status-row status-subtle tabular">
                  <span>{formatTime(s.createdAt)}</span>
                  {s.pages > 0 && <span>{t.scans.pages(s.pages)}</span>}
                </span>
              </li>
            ))}
          </ul>
          <div>
            <LinkButton variant="quiet" href="/scan">
              {t.scans.seeAll}
            </LinkButton>
          </div>
        </>
      ) : (
        !error && (
          <EmptyState icon="scan" title={t.scans.empty}>
            {t.scans.emptyBody}
          </EmptyState>
        )
      )}
    </Card>
  );
}

function reconcileLabel(item: string) {
  if (item.startsWith("workload/")) return t.reconcile.workload(item.slice("workload/".length));
  return t.reconcile.items[item] ?? item;
}

/** The settings file and the control plane's reconcile problems. */
function SystemCard({ status }: { status: StatusDTO }) {
  const { config } = status;
  const changed = formatTime(config.changedAt);
  const tone: Tone =
    config.state === "applied" ? "ok" : config.state === "invalid" ? "problem" : "idle";
  const problems = status.reconcile.filter((r) => !r.ok);
  return (
    <Card title={t.system.title}>
      <div className="status-row">
        <span>{t.system.configLabel}</span>
        <StatusBadge tone={tone}>{t.config.state[config.state]}</StatusBadge>
      </div>
      {changed && <p className="status-subtle">{t.config.changedAt(changed)}</p>}
      {config.state === "invalid" && config.errors.length > 0 && (
        <ul className="status-list">
          {config.errors.map((e, i) => (
            // biome-ignore lint/suspicious/noArrayIndexKey: errors are a fixed list per Status
            <li key={i}>{t.config.error(e.line, e.column, e.message)}</li>
          ))}
        </ul>
      )}
      {problems.length > 0 && (
        <section className="status-section">
          <h3>{t.reconcile.title}</h3>
          <ul className="status-list">
            {problems.map((p) => (
              <li key={p.item}>
                <span className="status-problem-item">{reconcileLabel(p.item)}</span>
                <span>{p.message}</span>
                <span className="status-subtle tabular">{formatTime(p.at)}</span>
              </li>
            ))}
          </ul>
        </section>
      )}
      <div>
        <LinkButton href="/settings">{t.config.openSettings}</LinkButton>
      </div>
    </Card>
  );
}

/**
 * Messages no device card explains, shown once above the cards: a workload that is not running
 * (failed, or waiting such as unschedulable) and an overall error message (an observe or apply
 * error). Text an enabled card already shows is left out. Only failures are alerts.
 */
function workloadProblems(
  status: StatusDTO,
  devices: (StatusPrinter | StatusScanner)[],
): { text: string; failed: boolean }[] {
  const multi = status.workloads.length > 1;
  const shown = new Set(devices.map((d) => d.message));
  const lines: { text: string; failed: boolean }[] = [];
  for (const w of status.workloads) {
    if (w.phase === "running" || !w.message || shown.has(w.message)) continue;
    shown.add(w.message);
    const text = multi && w.node ? `${t.workloadOn(w.node)}: ${w.message}` : w.message;
    lines.push({ text, failed: w.phase === "failed" });
  }
  if (status.phase === "error" && status.message && !shown.has(status.message))
    lines.push({ text: status.message, failed: true });
  return lines;
}

export function Status() {
  const status = useStatus();
  if (!status) {
    return (
      <div className="status-view">
        <PageHeader title={strings.nav.status} description={t.description} />
        <p className="status-subtle">{strings.loading}</p>
      </div>
    );
  }
  const printers = enabled(status.printers);
  const scanners = enabled(status.scanners);
  const problems = workloadProblems(status, [...printers, ...scanners]);
  const failures = problems.filter((p) => p.failed);
  return (
    <div className="status-view">
      <PageHeader title={strings.nav.status} description={t.description} />
      {failures.length > 0 && (
        <InlineError>
          <p className="status-error-title">{t.workloadError}</p>
          {failures.map((p) => (
            <p key={p.text}>{p.text}</p>
          ))}
        </InlineError>
      )}
      {problems
        .filter((p) => !p.failed)
        .map((p) => (
          <p key={p.text} className="status-message">
            {p.text}
          </p>
        ))}
      <div className="status-layout">
        <div className="status-main">
          {printers.length + scanners.length === 0 ? (
            <Card>
              <EmptyState
                icon="status"
                title={t.empty.title}
                action={
                  <LinkButton variant="primary" href="/setup?add=1">
                    {t.empty.detect}
                  </LinkButton>
                }
              >
                {t.empty.body}
              </EmptyState>
            </Card>
          ) : (
            <>
              {printers.map((p) => (
                <PrinterCard key={p.id} printer={p} status={status} />
              ))}
              {scanners.map((s) => (
                <ScannerCard key={s.id} scanner={s} status={status} />
              ))}
            </>
          )}
        </div>
        <div className="status-side">
          {printers.length > 0 && <JobsCard printers={printers} />}
          {scanners.length > 0 && <ScansCard scanners={scanners} />}
          <SystemCard status={status} />
        </div>
      </div>
    </div>
  );
}
