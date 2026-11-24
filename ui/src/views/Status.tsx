// The Status dashboard: device, printer, scanner, jobs, config, problems and how to connect.
import { useEffect, useState } from "react";
import { problemMessage } from "../api/client";
import { useJobs, useStatus } from "../api/events";
import {
  Button,
  Card,
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
import type { Job, Status as StatusDTO } from "../gen/api";
import { strings } from "../strings";
import { statusStrings as t } from "./Status.strings";
import "./Status.css";

const RECENT_JOBS = 5;

const phaseTone: Record<StatusDTO["device"]["phase"], Tone> = {
  none: "idle",
  probing: "busy",
  deploying: "busy",
  ready: "ok",
  degraded: "problem",
  error: "problem",
};

const endpointTone: Record<StatusDTO["printer"]["state"], Tone> = {
  idle: "ok",
  processing: "busy",
  stopped: "problem",
  unknown: "idle",
};

const jobTone: Record<Job["state"], Tone> = {
  pending: "busy",
  processing: "busy",
  completed: "ok",
  canceled: "idle",
  aborted: "problem",
  held: "idle",
  stopped: "problem",
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

function DeviceCard({ device }: { device: StatusDTO["device"] }) {
  const d = device.decision;
  const usb = d?.device;
  const family = (id: string) => t.device.families[id] ?? id;
  return (
    <Card
      title={t.device.title}
      actions={
        <StatusBadge tone={phaseTone[device.phase]} pulse={phaseTone[device.phase] === "busy"}>
          {t.device.phase[device.phase]}
        </StatusBadge>
      }
    >
      {device.message && device.phase !== "error" && (
        <p className="status-message">{device.message}</p>
      )}
      <dl className="status-facts">
        <div>
          <dt>{t.device.modeLabel}</dt>
          <dd>{t.device.mode[device.mode]}</dd>
        </div>
        <div>
          <dt>{t.device.deviceLabel}</dt>
          <dd>
            {!d ? (
              t.device.noDecision
            ) : !usb ? (
              t.device.simulatedDevice
            ) : (
              <>
                <span>{`${usb.manufacturer} ${usb.product}`.trim() || t.device.unnamedDevice}</span>
                <span className="status-subtle">
                  {[
                    t.device.usbIds(usb.vendorId, usb.productId),
                    usb.serial && t.device.serial(usb.serial),
                  ]
                    .filter(Boolean)
                    .join(", ")}
                </span>
              </>
            )}
          </dd>
        </div>
        {d && (
          <>
            <div>
              <dt>{t.device.ruleLabel}</dt>
              <dd>{d.rule}</dd>
            </div>
            <div>
              <dt>{t.device.printerFamilyLabel}</dt>
              <dd>{d.printer ? family(d.printer) : t.device.noFamily}</dd>
            </div>
            <div>
              <dt>{t.device.scannerFamilyLabel}</dt>
              <dd>{d.scanner ? family(d.scanner) : t.device.noFamily}</dd>
            </div>
          </>
        )}
      </dl>
      {device.blobs.length > 0 && (
        <section className="status-section">
          <h3>{t.device.blobsTitle}</h3>
          <ul className="status-list">
            {device.blobs.map((b) => (
              <li key={b.id}>
                <span className="status-row">
                  <span>{`${b.id} ${b.version}`}</span>
                  <StatusBadge tone={blobTone[b.state]}>{t.device.blobState[b.state]}</StatusBadge>
                </span>
                {b.message && <span className="status-subtle">{b.message}</span>}
              </li>
            ))}
          </ul>
        </section>
      )}
      <section className="status-section">
        <h3>{t.advertised.title}</h3>
        {device.advertised.length === 0 ? (
          <p className="status-subtle">{t.advertised.notDiscoverable}</p>
        ) : (
          <>
            <p className="status-check">
              <Icon name="check" size={18} />
              {t.advertised.discoverable}
            </p>
            <ul className="status-list">
              {device.advertised.map((s, i) => (
                // Avahi can list one service once per interface, so names repeat.
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
    </Card>
  );
}

function PrinterCard({ printer }: { printer: StatusDTO["printer"] }) {
  const reasons = [
    ...new Set(printer.reasons.map(reasonText).filter((r): r is string => r !== null)),
  ];
  return (
    <Card
      title={t.printer.title}
      actions={
        <StatusBadge tone={endpointTone[printer.state]}>
          {t.printer.state[printer.state]}
        </StatusBadge>
      }
    >
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
          <LinkButton href="/print">{t.printer.open}</LinkButton>
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
    </Card>
  );
}

function ScannerCard({ scanner }: { scanner: StatusDTO["scanner"] }) {
  return (
    <Card
      title={t.scanner.title}
      actions={
        <StatusBadge tone={endpointTone[scanner.state]}>
          {t.scanner.state[scanner.state]}
        </StatusBadge>
      }
    >
      {scanner.available ? (
        <div>
          <LinkButton href="/scan">{t.scanner.open}</LinkButton>
        </div>
      ) : (
        <p className="status-subtle">{t.scanner.unavailable}</p>
      )}
    </Card>
  );
}

function JobsCard({ available }: { available: boolean }) {
  const { jobs, error } = useJobs();
  const recent = jobs.slice(0, RECENT_JOBS);
  return (
    <Card
      title={t.jobs.title}
      actions={
        jobs.length > 0 && (
          <LinkButton variant="quiet" href="/print">
            {t.jobs.seeAll}
          </LinkButton>
        )
      }
    >
      {available && error ? <InlineError>{problemMessage(error)}</InlineError> : null}
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
      ) : (
        <EmptyState icon="print" title={t.jobs.empty}>
          {available ? t.jobs.emptyBody : t.jobs.unavailable}
        </EmptyState>
      )}
    </Card>
  );
}

function ConfigCard({ config }: { config: StatusDTO["config"] }) {
  const changed = formatTime(config.changedAt);
  const tone: Tone =
    config.state === "applied" ? "ok" : config.state === "invalid" ? "problem" : "idle";
  return (
    <Card
      title={t.config.title}
      actions={<StatusBadge tone={tone}>{t.config.state[config.state]}</StatusBadge>}
    >
      {changed && <p className="status-subtle">{t.config.changedAt(changed)}</p>}
      {config.state === "invalid" && config.errors.length > 0 && (
        <ul className="status-list">
          {config.errors.map((e, i) => (
            // biome-ignore lint/suspicious/noArrayIndexKey: errors are a fixed list per Status
            <li key={i}>{t.config.error(e.line, e.column, e.message)}</li>
          ))}
        </ul>
      )}
      <div>
        <LinkButton href="/settings">{t.config.openSettings}</LinkButton>
      </div>
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

function ConnectCard({ connect }: { connect: StatusDTO["connect"] }) {
  const name = connect.dnssdName || t.connect.thisPrinter;
  return (
    <Card title={t.connect.title} className="status-connect">
      <p className="status-subtle">{t.connect.intro}</p>
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
              {t.connect.steps[k](name).map((step) => (
                <li key={step}>{step}</li>
              ))}
            </ol>
          </TabPanel>
        ))}
      </Tabs>
      <section className="status-section">
        <h3>{t.connect.manualTitle}</h3>
        <dl className="status-addresses">
          <Addresses label={t.connect.printerAddress} values={connect.ippUris} />
          <Addresses label={t.connect.scannerAddress} values={connect.esclUrls} />
        </dl>
        <p className="status-subtle">{t.connect.tailscale}</p>
      </section>
    </Card>
  );
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
  const problems = status.reconcile.filter((r) => !r.ok);
  return (
    <div className="status-view">
      <PageHeader title={strings.nav.status} description={t.description} />
      {status.device.phase === "error" && (
        <InlineError>
          <p className="status-error-title">{t.deviceError}</p>
          {status.device.message && <p>{status.device.message}</p>}
        </InlineError>
      )}
      <div className="status-grid">
        <DeviceCard device={status.device} />
        <PrinterCard printer={status.printer} />
        <ScannerCard scanner={status.scanner} />
        <JobsCard available={status.printer.available} />
        <ConfigCard config={status.config} />
        {problems.length > 0 && (
          <Card title={t.reconcile.title} tone="problem">
            <ul className="status-list">
              {problems.map((p) => (
                <li key={p.item}>
                  <span className="status-problem-item">{t.reconcile.items[p.item] ?? p.item}</span>
                  <span>{p.message}</span>
                  <span className="status-subtle tabular">{formatTime(p.at)}</span>
                </li>
              ))}
            </ul>
          </Card>
        )}
        <ConnectCard connect={status.connect} />
      </div>
    </div>
  );
}
