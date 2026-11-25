// The setup wizard, for the first run and for "Detect and add devices" (/setup?add=1). Four
// steps choose devices and build their entries from setup.devices; then the wizard follows Status
// until the devices run, offers their defaults from the live capabilities, and shows how to
// connect. The current form step lives in the URL hash.
import { useEffect, useState } from "react";
import { api, ConflictError, ProblemError, problemMessage } from "../api/client";
import { useStatus } from "../api/events";
import { acceptedSha, EulaAccept, withAccepted } from "../components/EulaAccept";
import { OptionField } from "../components/OptionField";
import {
  Button,
  Card,
  DeviceStatusBadge,
  Disclosure,
  InlineError,
  LinkButton,
  PageHeader,
  ProgressBar,
  RadioGroup,
  Select,
  TextField,
} from "../components/ui";
import {
  type Config,
  ConfigSchema,
  type DetectedDevice,
  type EULARef,
  type IgnoredDevice,
  type Printer,
  type PrinterDecision,
  PrinterSchema,
  type Scanner,
  type ScannerDecision,
  ScannerSchema,
  type Setup as SetupDTO,
  type Status,
  type StatusPrinter,
  type StatusScanner,
  type USBDevice,
} from "../gen/api";
import {
  type FieldKind,
  type OptionValue,
  optionState,
  printerGroups,
  scannerGroups,
  setDefault,
} from "../lib/options";
import { navigate, onNextFrameUnlessUserActs, useQueryParam } from "../router";
import { strings } from "../strings";
import { hasPrinterInterface, isNeverPrinterOrScanner } from "../usb";
import { setupStrings as s } from "./Setup.strings";
import "./Setup.css";

const STEPS = ["devices", "names", "drivers", "review"] as const;
type Step = (typeof STEPS)[number];
/** The progress list also shows the defaults step, which follows once the devices run. */
const PROGRESS = [...STEPS, "defaults"] as const;

const A4 = "iso_a4_210x297mm";
const LETTER = "na_letter_8.5x11in";

/** The paper size people in this locale expect: Letter in the US and Canada, A4 elsewhere. */
export function mediaForLanguage(lang: string): string {
  return /^en-(us|ca)$/i.test(lang) ? LETTER : A4;
}

function stepFromHash(): Step {
  const h = window.location.hash.slice(1);
  return (STEPS as readonly string[]).includes(h) ? (h as Step) : "devices";
}

const family = (id: string) => strings.device.families[id] ?? id;

// Module level, so the reload timer is not restarted by every render.
const reloadApp = () => window.location.assign("/");

type Role = "printer" | "scanner";
const ROLES: Role[] = ["printer", "scanner"];

/** One detected device and what each of its roles is: offered now, in use, or not offered. */
type Row = { key: string; d: DetectedDevice; offered: Role[]; inUse: Role[]; notOffered: Role[] };

const suggestionOf = (d: DetectedDevice, role: Role) =>
  role === "printer" ? d.suggestedPrinter : d.suggestedScanner;
const configuredId = (d: DetectedDevice, role: Role) =>
  role === "printer" ? d.printerId : d.scannerId;

// Choices are keyed by the device's identity, not its list position or USB address, so a refetch
// or a replug keeps them. Two serial-less devices of one model are told apart by their order.
function rows(devices: DetectedDevice[], offerAgain: string[]): Row[] {
  const seen = new Map<string, number>();
  return devices.map((d) => {
    const id = `${d.node}|${d.device.vendorId}:${d.device.productId}:${d.device.serial}`;
    const n = (seen.get(id) ?? 0) + 1;
    seen.set(id, n);
    const key = n === 1 ? id : `${id}#${n}`;
    const inUse = ROLES.filter((r) => configuredId(d, r) !== "");
    const free = ROLES.filter((r) => !inUse.includes(r) && suggestionOf(d, r) !== null);
    const notOffered = free.filter((r) => d.ignored.includes(r) && !offerAgain.includes(key));
    return { key, d, inUse, notOffered, offered: free.filter((r) => !notOffered.includes(r)) };
  });
}

type Edit = {
  name?: string;
  location?: string;
  driver?: Printer["driver"];
  backend?: Scanner["backend"];
};

/** The user's choices. Everything else follows the latest setup data. */
type Draft = {
  simulated: boolean;
  /** `<device key>:<role>` → false for "Don't use"; absent means use. */
  use: Record<string, boolean>;
  offerAgain: string[];
  edits: Record<string, Edit>;
  /** The installation name (first run); null follows the first chosen device. */
  title: string | null;
  /** Accepted licences; null keeps the current config's. */
  accepted: EULARef[] | null;
};

const emptyDraft: Draft = {
  simulated: false,
  use: {},
  offerAgain: [],
  edits: {},
  title: null,
  accepted: null,
};

type Chosen =
  | { ek: string; role: "printer"; entry: Printer; decision: PrinterDecision | null }
  | { ek: string; role: "scanner"; entry: Scanner; decision: ScannerDecision | null };

const cut = (text: string, n: number) => [...text].slice(0, n).join("");

function simulatedEntries(name: string): [Printer, Scanner] {
  const common = { location: "", enabled: true, mode: "simulated" as const, defaults: {} };
  return [
    { ...common, id: "printer", name, driver: "auto" },
    { ...common, id: "scanner", name: cut(`${name} scanner`, 63), backend: "auto" },
  ];
}

function withEdit<T extends Printer | Scanner>(entry: T, e: Edit | undefined): T {
  if (!e) return entry;
  const { driver, backend, ...common } = e;
  return {
    ...entry,
    ...common,
    ...("driver" in entry && driver && { driver }),
    ...("backend" in entry && backend && { backend }),
  };
}

function chosenEntries(setup: SetupDTO, draft: Draft, all: Row[]): Chosen[] {
  if (draft.simulated) {
    const [p, sc] = simulatedEntries(setup.suggested.name);
    return [
      {
        ek: "sim:printer",
        role: "printer",
        entry: withEdit(p, draft.edits["sim:printer"]),
        decision: null,
      },
      {
        ek: "sim:scanner",
        role: "scanner",
        entry: withEdit(sc, draft.edits["sim:scanner"]),
        decision: null,
      },
    ];
  }
  const out: Chosen[] = [];
  for (const r of all) {
    for (const role of r.offered) {
      const ek = `${r.key}:${role}`;
      if (draft.use[ek] === false) continue;
      if (role === "printer" && r.d.suggestedPrinter)
        out.push({
          ek,
          role,
          entry: withEdit(r.d.suggestedPrinter, draft.edits[ek]),
          decision: r.d.printerDecision,
        });
      if (role === "scanner" && r.d.suggestedScanner)
        out.push({
          ek,
          role,
          entry: withEdit(r.d.suggestedScanner, draft.edits[ek]),
          decision: r.d.scannerDecision,
        });
    }
  }
  return out;
}

const sameDevice = (i: IgnoredDevice, u: USBDevice) =>
  i.vendorId === u.vendorId && i.productId === u.productId && (i.serial === u.serial || !i.serial);

function ignore(list: IgnoredDevice[], u: USBDevice, role: Role): IgnoredDevice[] {
  if (list.some((i) => sameDevice(i, u) && i.roles.includes(role))) return list;
  const hit = list.find(
    (i) => i.vendorId === u.vendorId && i.productId === u.productId && i.serial === u.serial,
  );
  if (hit) return list.map((i) => (i === hit ? { ...i, roles: [...i.roles, role] } : i));
  return [
    ...list,
    { vendorId: u.vendorId, productId: u.productId, serial: u.serial, roles: [role] },
  ];
}

/** Without `role` for `u`; an entry left with no role is dropped (roles may not be empty). */
function unignore(list: IgnoredDevice[], u: USBDevice, role: Role): IgnoredDevice[] {
  return list.flatMap((i) => {
    if (!sameDevice(i, u)) return [i];
    const roles = i.roles.filter((r) => r !== role);
    return roles.length ? [{ ...i, roles }] : [];
  });
}

/** The licences the chosen printers need or offer: the only ones the wizard asks about. */
function shownEulas(setup: SetupDTO, chosen: Chosen[]) {
  const ids = new Set(
    chosen.flatMap((c) =>
      c.role === "printer" && c.decision
        ? [...c.decision.requiredEulas, ...c.decision.optionalEulas]
        : [],
    ),
  );
  return setup.eulas.filter((e) => ids.has(e.id));
}

const eulaKey = (r: { id: string; version: string }) => `${r.id}@${r.version}`;

type Added = { printers: string[]; scanners: string[] };

/**
 * `base` with the chosen entries appended (ids made unique), "Don't use" recorded in
 * ignoredDevices, used roles taken out of it, and the licences the wizard showed.
 */
function compose(
  base: Config,
  setup: SetupDTO,
  draft: Draft,
  all: Row[],
  chosen: Chosen[],
): { config: Config; added: Added } {
  const taken = new Set([...base.printers, ...base.scanners].map((e) => e.id));
  const unique = (id: string) => {
    let out = id;
    for (let i = 2; taken.has(out); i++) out = `${id.slice(0, 31 - String(i).length)}-${i}`;
    taken.add(out);
    return out;
  };
  const printers = [...base.printers];
  const scanners = [...base.scanners];
  for (const c of chosen) {
    if (c.role === "printer") printers.push({ ...c.entry, id: unique(c.entry.id) });
    else scanners.push({ ...c.entry, id: unique(c.entry.id) });
  }
  let ignoredDevices = base.ignoredDevices;
  if (!draft.simulated) {
    for (const r of all) {
      for (const role of r.offered) {
        ignoredDevices =
          draft.use[`${r.key}:${role}`] === false
            ? ignore(ignoredDevices, r.d.device, role)
            : unignore(ignoredDevices, r.d.device, role);
      }
    }
  }
  const shown = new Set(shownEulas(setup, chosen).map(eulaKey));
  const accepted = draft.accepted ?? base.licenses.acceptedEulas;
  const acceptedEulas = [
    ...base.licenses.acceptedEulas.filter((r) => !shown.has(eulaKey(r))),
    ...accepted.filter((r) => shown.has(eulaKey(r))),
  ];
  return {
    config: { ...base, printers, scanners, ignoredDevices, licenses: { acceptedEulas } },
    added: {
      printers: printers.slice(base.printers.length).map((p) => p.id),
      scanners: scanners.slice(base.scanners.length).map((sc) => sc.id),
    },
  };
}

/** Text for an error from saving: the server's problem, or the wizard's own message. */
function errorText(err: unknown): string {
  if (err instanceof ProblemError || err instanceof ConflictError) return problemMessage(err);
  return err instanceof Error ? err.message : problemMessage(err);
}

export type SetupProps = {
  /** How long to wait for the devices to become ready before saying so. */
  readyTimeoutMs?: number;
  /** Called after a first-run 412 (another session created the config first). */
  reload?: () => void;
  reloadDelayMs?: number;
};

export function Setup({
  readyTimeoutMs = 180_000,
  reload = reloadApp,
  reloadDelayMs = 3000,
}: SetupProps = {}) {
  const status = useStatus();
  const addQuery = useQueryParam("add") === "1";
  const [setup, setSetup] = useState<SetupDTO | null>(null);
  const [loadError, setLoadError] = useState<unknown>(null);
  const [attempt, setAttempt] = useState(0);
  // Add mode: the config the new devices join (its ids, ignored devices and licences).
  const [current, setCurrent] = useState<Config | null>(null);
  const [configError, setConfigError] = useState<unknown>(null);
  const [draft, setDraft] = useState<Draft>(emptyDraft);
  const [step, setStep] = useState<Step>("devices");
  const [tried, setTried] = useState(false);
  const [flow, setFlow] = useState<"form" | "applying" | "defaults" | "done">("form");
  const [added, setAdded] = useState<Added>({ printers: [], scanners: [] });
  const [submitting, setSubmitting] = useState(false);
  const [submitError, setSubmitError] = useState<unknown>(null);
  const [conflict, setConflict] = useState(false);
  const [timedOut, setTimedOut] = useState(false);
  // "idle", "asked", or the error POST /setup/detect answered.
  const [detect, setDetect] = useState<unknown>("idle");

  // The draft is not kept across reloads, so a reload starts again at the first step.
  useEffect(() => {
    if (window.location.hash)
      window.history.replaceState(null, "", window.location.pathname + window.location.search);
    const onPop = () => setStep(stepFromHash());
    window.addEventListener("popstate", onPop);
    return () => window.removeEventListener("popstate", onPop);
  }, []);

  // GET /setup has no event of its own: refetch whenever the detected devices change.
  const refreshKey = JSON.stringify([
    status?.phase,
    status?.message,
    status?.devices.map((d) => [
      d.node,
      d.device.bus,
      d.device.address,
      d.device.serial,
      d.printerId,
      d.scannerId,
      d.ignored,
      d.canPrint,
      d.canScan,
    ]),
  ]);
  // biome-ignore lint/correctness/useExhaustiveDependencies: refreshKey and attempt are refetch triggers
  useEffect(() => {
    if (flow !== "form") return;
    let live = true;
    api.getSetup().then(
      (r) => {
        if (!live) return;
        setSetup(r);
        setLoadError(null);
      },
      (err) => {
        if (live) setLoadError(err);
      },
    );
    return () => {
      live = false;
    };
  }, [refreshKey, attempt, flow]);

  const adding = setup !== null && !setup.required && addQuery;
  // biome-ignore lint/correctness/useExhaustiveDependencies: attempt is the retry trigger
  useEffect(() => {
    if (!adding) return;
    let live = true;
    setConfigError(null);
    api.getConfig().then(
      (r) => {
        if (!live) return;
        if (r.config) setCurrent(r.config);
        else setConfigError(new Error(s.configInvalid));
      },
      (err) => {
        if (live) setConfigError(err);
      },
    );
    return () => {
      live = false;
    };
  }, [adding, attempt]);

  const allIds = [...added.printers, ...added.scanners];
  const statusOf = (id: string) =>
    status?.printers.find((p) => p.id === id) ?? status?.scanners.find((x) => x.id === id);
  // New entries appear in Status only once their config is applied.
  const running =
    status?.config.state === "applied" &&
    allIds.every((id) => {
      const phase = statusOf(id)?.phase;
      return phase === "ready" || phase === "degraded";
    });
  // With nothing added, the empty Status is the next page, once it no longer says "missing"
  // (the shell sends every page back here while the config is missing).
  const nothingAdded = allIds.length === 0;
  useEffect(() => {
    if (flow !== "applying" || !running) return;
    if (nothingAdded) navigate("/", { force: true });
    else setFlow("defaults");
  }, [flow, running, nothingAdded]);

  useEffect(() => {
    if (flow !== "applying") return;
    const t = setTimeout(() => setTimedOut(true), readyTimeoutMs);
    return () => clearTimeout(t);
  }, [flow, readyTimeoutMs]);

  useEffect(() => {
    if (!conflict || adding) return;
    const t = setTimeout(reload, reloadDelayMs);
    return () => clearTimeout(t);
  }, [conflict, adding, reload, reloadDelayMs]);

  // Later stages replace the pressed button, so move focus to their heading.
  useEffect(() => {
    if (flow !== "form") document.querySelector<HTMLElement>(".setup-view h1")?.focus();
  }, [flow]);

  if (flow === "applying") return <Applying status={status} ids={allIds} timedOut={timedOut} />;
  if (flow === "defaults")
    return (
      <DefaultsStep
        status={status}
        added={added}
        title={addQuery ? s.addTitle : s.title}
        onDone={() => setFlow("done")}
      />
    );
  if (flow === "done") return <Done status={status} added={added} />;

  const base = adding ? current : setup?.suggested;
  const failed = loadError ?? (adding ? configError : null);
  if (!setup || !base) {
    return (
      <div className="setup-view">
        <PageHeader title={strings.nav.setup} />
        {failed ? (
          <InlineError
            action={
              <Button onPress={() => setAttempt((a) => a + 1)}>{strings.buttons.retry}</Button>
            }
          >
            <p>{s.loadFailed}</p>
            <p>{errorText(failed)}</p>
          </InlineError>
        ) : (
          <p role="status">{strings.loading}</p>
        )}
      </div>
    );
  }

  if (!setup.required && !adding && !conflict) {
    return (
      <div className="setup-view">
        <PageHeader
          title={s.alreadyDone.title}
          description={s.alreadyDone.body}
          actions={
            <>
              <LinkButton href="/setup?add=1">{s.alreadyDone.add}</LinkButton>
              <LinkButton href="/settings" variant="primary">
                {s.alreadyDone.action}
              </LinkButton>
            </>
          }
        />
      </div>
    );
  }

  const all = rows(setup.devices, draft.offerAgain);
  const chosen = chosenEntries(setup, draft, all);
  // Prefilled with the first chosen device's own name, not with edits to that device's name.
  const title =
    draft.title ??
    chosenEntries(setup, { ...draft, edits: {} }, all)[0]?.entry.name ??
    setup.suggested.name;
  const change = (fn: (d: Draft) => Draft) => setDraft(fn);
  const edit = (ek: string, e: Edit) =>
    change((d) => ({ ...d, edits: { ...d.edits, [ek]: { ...d.edits[ek], ...e } } }));
  const idx = STEPS.indexOf(step);
  const go = (to: Step) => {
    setTried(false);
    setStep(to);
    navigate(`#${to}`);
  };

  const canContinue =
    step !== "devices" ||
    draft.simulated ||
    all.some((r) => r.offered.length + r.inUse.length + r.notOffered.length > 0);

  const takenNames = (adding ? base.printers : []).map((p) => p.name.toLowerCase());
  const chosenNames = chosen
    .filter((c) => c.role === "printer")
    .map((c) => c.entry.name.toLowerCase());
  const nameError = (c: Chosen) => {
    if (!PrinterSchema.shape.name.safeParse(c.entry.name).success) return s.names.nameInvalid;
    const n = c.entry.name.toLowerCase();
    if (
      c.role === "printer" &&
      (takenNames.includes(n) || chosenNames.filter((x) => x === n).length > 1)
    )
      return s.names.nameTaken;
    return undefined;
  };
  const locationOk = (c: Chosen) =>
    PrinterSchema.shape.location.safeParse(c.entry.location).success;
  const titleOk = adding || ConfigSchema.shape.name.safeParse(title).success;
  const namesOk = titleOk && chosen.every((c) => !nameError(c) && locationOk(c));

  const next = () => {
    if (step === "names" && !namesOk) {
      setTried(true);
      // After the error messages render, take keyboard users to the first one.
      onNextFrameUnlessUserActs(() =>
        document.querySelector<HTMLElement>(".setup-view [aria-invalid='true']")?.focus(),
      );
      return;
    }
    const to = STEPS[idx + 1];
    if (to) go(to);
  };

  const finish = async () => {
    if (submitting) return;
    setSubmitting(true);
    setSubmitError(null);
    setConflict(false);
    try {
      let result: Added;
      if (adding) {
        // Join the file as it is now, never the copy read when the wizard opened.
        const now = await api.getConfig();
        if (!now.config) throw new Error(s.configInvalid);
        const { config, added: a } = compose(now.config, setup, draft, all, chosen);
        const { printers, scanners, ignoredDevices, licenses } = config;
        await api.patchConfig({ printers, scanners, ignoredDevices, licenses }, now.etag);
        result = a;
      } else {
        const { config, added: a } = compose(
          { ...setup.suggested, name: title },
          setup,
          draft,
          all,
          chosen,
        );
        await api.createConfig(config);
        result = a;
      }
      setAdded(result);
      setFlow("applying");
    } catch (err) {
      if (err instanceof ConflictError) setConflict(true);
      else setSubmitError(err);
    } finally {
      setSubmitting(false);
    }
  };

  const askDetect = () => {
    setDetect("asked");
    api.detect().catch((err) => setDetect(err));
  };

  return (
    <div className="setup-view">
      <Progress current={step} title={adding ? s.addTitle : s.title} />
      <PageHeader
        title={s.steps[step]}
        description={descriptions[step]}
        documentTitle={s.stepTitle(idx + 1, PROGRESS.length, s.steps[step])}
      />

      {step === "devices" && (
        <Devices
          setup={setup}
          status={status}
          all={all}
          draft={draft}
          change={change}
          detect={detect}
          onDetect={askDetect}
        />
      )}
      {step === "names" && (
        <>
          {!adding && (
            <Card>
              <TextField
                label={s.names.titleLabel}
                description={s.names.titleHint}
                value={title}
                onChange={(t) => change((d) => ({ ...d, title: t }))}
                errorMessage={tried && !titleOk ? s.names.nameInvalid : undefined}
                isRequired
              />
            </Card>
          )}
          {chosen.length === 0 && <p>{s.names.nothingChosen}</p>}
          {chosen.map((c) => (
            <Card
              key={c.ek}
              title={
                c.role === "printer"
                  ? s.names.printerTitle(c.entry.name)
                  : s.names.scannerTitle(c.entry.name)
              }
            >
              <TextField
                label={s.names.nameLabel}
                description={c.role === "printer" ? s.names.printerHint : s.names.scannerHint}
                value={c.entry.name}
                onChange={(name) => edit(c.ek, { name })}
                errorMessage={tried ? nameError(c) : undefined}
                isRequired
              />
              <TextField
                label={s.names.locationLabel}
                description={s.names.locationHint}
                value={c.entry.location}
                onChange={(location) => edit(c.ek, { location })}
                errorMessage={tried && !locationOk(c) ? s.names.locationInvalid : undefined}
              />
            </Card>
          ))}
        </>
      )}
      {step === "drivers" && (
        <Drivers
          setup={setup}
          draft={draft}
          chosen={chosen}
          accepted={draft.accepted ?? base.licenses.acceptedEulas}
          edit={edit}
          setAccepted={(accepted) => change((d) => ({ ...d, accepted }))}
        />
      )}
      {step === "review" && (
        <Review
          setup={setup}
          draft={draft}
          all={all}
          chosen={chosen}
          title={adding ? undefined : title}
          accepted={draft.accepted ?? base.licenses.acceptedEulas}
        />
      )}

      {conflict && (
        <InlineError
          action={
            adding ? (
              <Button onPress={finish}>{s.review.saveAgain}</Button>
            ) : (
              <Button onPress={reload}>{s.review.reloadNow}</Button>
            )
          }
        >
          <p>{adding ? s.review.addConflict : s.review.conflict}</p>
        </InlineError>
      )}
      {submitError !== null && (
        <InlineError>
          {submitError instanceof ProblemError && submitError.errors.length > 0 ? (
            <>
              <p>{s.review.invalid}</p>
              <ul>
                {submitError.errors.map((e) => (
                  <li key={e.path + e.message}>
                    {e.path}: {e.message}
                  </li>
                ))}
              </ul>
            </>
          ) : (
            <p>{errorText(submitError)}</p>
          )}
        </InlineError>
      )}

      <div className="setup-actions">
        {idx > 0 && (
          <Button onPress={() => go(STEPS[idx - 1] ?? "devices")}>{strings.buttons.back}</Button>
        )}
        {step === "review" ? (
          // No isPending: the button unmounts on success, and React Aria's pending announcement
          // would be left pointing at nothing.
          <Button variant="primary" onPress={finish} isDisabled={conflict && !adding}>
            {submitting ? s.review.finishing : s.review.finish}
          </Button>
        ) : (
          <Button variant="primary" onPress={next} isDisabled={!canContinue}>
            {strings.buttons.next}
          </Button>
        )}
      </div>
    </div>
  );
}

const descriptions: Record<Step, string> = {
  devices: s.detect.description,
  names: s.names.description,
  drivers: s.drivers.description,
  review: s.review.description,
};

function Progress({ current, title }: { current: (typeof PROGRESS)[number]; title: string }) {
  const idx = PROGRESS.indexOf(current);
  return (
    <>
      <p className="setup-eyebrow">
        {title} · {s.stepOf(idx + 1, PROGRESS.length)}
      </p>
      <ol className="setup-steps" aria-label={s.progressLabel}>
        {PROGRESS.map((st, i) => (
          <li
            key={st}
            aria-current={st === current ? "step" : undefined}
            data-done={i < idx || undefined}
          >
            <span className="setup-step-number tabular" aria-hidden="true">
              {i + 1}
            </span>
            <span className="setup-step-label">{s.steps[st]}</span>
          </li>
        ))}
      </ol>
    </>
  );
}

function deviceName(d: DetectedDevice, simulated: boolean) {
  const name = d.device.product || s.detect.unnamedDevice;
  return simulated ? s.detect.simulatedName(name) : name;
}

function DeviceText({ d }: { d: DetectedDevice }) {
  const dev = d.device;
  const simulated =
    d.suggestedPrinter?.mode === "simulated" || d.suggestedScanner?.mode === "simulated";
  return (
    <>
      <span className="setup-device-name">{deviceName(d, simulated)}</span>
      <span className="setup-device-meta">
        {[
          dev.manufacturer,
          s.detect.usbIds(dev.vendorId, dev.productId),
          dev.serial && s.detect.serial(dev.serial),
        ]
          .filter(Boolean)
          .join(" · ")}
      </span>
    </>
  );
}

function Devices({
  setup,
  status,
  all,
  draft,
  change,
  detect,
  onDetect,
}: {
  setup: SetupDTO;
  status: Status | null;
  all: Row[];
  draft: Draft;
  change: (fn: (d: Draft) => Draft) => void;
  detect: unknown;
  onDetect: () => void;
}) {
  const message = status?.message ?? "";
  const simButton = (primary: boolean) => (
    <Button
      variant={primary ? "primary" : "secondary"}
      onPress={() => change((d) => ({ ...d, simulated: true }))}
    >
      {s.detect.useSimulated}
    </Button>
  );
  const nameOf = (role: Role, id: string) =>
    (role === "printer" ? status?.printers : status?.scanners)?.find((e) => e.id === id)?.name ??
    id;

  if (draft.simulated) {
    return (
      <Card title={s.detect.simulatedTitle}>
        <p>{s.detect.simulatedBody}</p>
        {setup.usbResourceConfigured && (
          <div>
            <Button variant="quiet" onPress={() => change((d) => ({ ...d, simulated: false }))}>
              {s.detect.useUsb}
            </Button>
          </div>
        )}
      </Card>
    );
  }

  if (!setup.usbResourceConfigured) {
    return (
      <Card title={s.detect.noUsbTitle}>
        {setup.backend === "k8s" ? (
          <>
            <p>{s.detect.k8sIntro}</p>
            <ul>
              {s.detect.k8sSteps.map((t) => (
                <li key={t}>{t}</li>
              ))}
            </ul>
          </>
        ) : (
          <p>{s.detect.simBackend}</p>
        )}
        <p>{s.detect.simulatedOffer}</p>
        <div>{simButton(true)}</div>
      </Card>
    );
  }

  const detectAgain = (
    <div className="setup-detect">
      <Button onPress={onDetect}>{s.detect.detectAgain}</Button>
      {detect === "asked" && <p role="status">{s.detect.detecting}</p>}
      {detect !== "asked" && detect !== "idle" && (
        <InlineError>
          <p>{errorText(detect)}</p>
        </InlineError>
      )}
    </div>
  );

  if (all.length === 0) {
    return (
      <Card title={s.detect.searchingTitle}>
        <p role="status">{message || s.detect.waiting}</p>
        <p>{s.detect.plugInHint}</p>
        <div className="setup-row-actions">
          {simButton(false)}
          {detectAgain}
        </div>
      </Card>
    );
  }

  const roleless = (r: Row) => r.offered.length + r.inUse.length + r.notOffered.length === 0;
  const hidden = all.filter((r) => roleless(r) && isNeverPrinterOrScanner(r.d.device));
  const shown = all
    .filter((r) => !hidden.includes(r) && (r.offered.length || r.inUse.length || roleless(r)))
    .sort(
      (a, b) => Number(hasPrinterInterface(b.d.device)) - Number(hasPrinterInterface(a.d.device)),
    );
  const ignored = all.filter((r) => r.notOffered.length > 0);
  const usable = all.some((r) => !roleless(r));

  const choice = (r: Row, role: Role) => {
    const ek = `${r.key}:${role}`;
    return (
      <RadioGroup
        key={role}
        label={s.detect.roleLabel[role]}
        orientation="horizontal"
        options={[
          { id: "use", label: s.detect.use[role] },
          { id: "skip", label: s.detect.dontUse },
        ]}
        value={draft.use[ek] === false ? "skip" : "use"}
        onChange={(v) => change((d) => ({ ...d, use: { ...d.use, [ek]: v === "use" } }))}
      />
    );
  };
  const deviceList = (list: Row[]) => (
    <ul className="setup-devices">
      {list.map((r) => (
        <li key={r.key}>
          <DeviceText d={r.d} />
          {r.inUse.map((role) => (
            <p key={role} className="setup-device-use">
              {s.detect.inUse(role, nameOf(role, configuredId(r.d, role)))}
            </p>
          ))}
          {roleless(r) && <p className="setup-device-use">{s.detect.cannotUse}</p>}
          {r.offered.length > 0 && (
            <div className="setup-device-choices">{r.offered.map((role) => choice(r, role))}</div>
          )}
        </li>
      ))}
    </ul>
  );

  return (
    <>
      <Card title={s.detect.devicesTitle}>
        {!usable && (
          <>
            <p role="status">{message || s.detect.noUsable}</p>
            <p>{s.detect.plugInHint}</p>
            <div>{simButton(false)}</div>
          </>
        )}
        {shown.length > 0 && deviceList(shown)}
        {hidden.length > 0 && (
          <Disclosure title={s.detect.otherDevices(hidden.length)}>{deviceList(hidden)}</Disclosure>
        )}
        {detectAgain}
      </Card>
      {ignored.length > 0 && (
        <Card>
          <Disclosure title={s.detect.notOffered(ignored.length)}>
            <p>{s.detect.notOfferedHint}</p>
            <ul className="setup-devices">
              {ignored.map((r) => (
                <li key={r.key}>
                  <DeviceText d={r.d} />
                  <span className="setup-device-meta">{s.detect.ignoredRoles(r.notOffered)}</span>
                  <div>
                    <Button
                      variant="quiet"
                      onPress={() =>
                        change((d) => ({ ...d, offerAgain: [...d.offerAgain, r.key] }))
                      }
                    >
                      {s.detect.offerAgain}
                    </Button>
                  </div>
                </li>
              ))}
            </ul>
          </Disclosure>
        </Card>
      )}
    </>
  );
}

function Drivers({
  setup,
  draft,
  chosen,
  accepted,
  edit,
  setAccepted,
}: {
  setup: SetupDTO;
  draft: Draft;
  chosen: Chosen[];
  accepted: EULARef[];
  edit: (ek: string, e: Edit) => void;
  setAccepted: (list: EULARef[]) => void;
}) {
  const eulas = shownEulas(setup, chosen);
  const auto = (d: { family: string } | null) =>
    d ? s.drivers.automatic(family(d.family)) : s.drivers.automaticUnknown;

  return (
    <>
      {draft.simulated ? (
        <Card>
          <p>{s.drivers.simulated}</p>
        </Card>
      ) : (
        chosen.map((c) =>
          c.role === "printer" ? (
            <Card key={c.ek} title={s.names.printerTitle(c.entry.name)}>
              {c.decision?.rule === "none" ? (
                <NoDriver device={c.decision.device} hint={s.drivers.noDriverHint} />
              ) : (
                c.decision && <p>{s.drivers.explained(family(c.decision.family))}</p>
              )}
              <Select
                label={s.drivers.driverLabel}
                options={PrinterSchema.shape.driver.options.map((id) => ({
                  id,
                  label:
                    id !== "auto"
                      ? family(id)
                      : c.decision?.rule === "none"
                        ? s.drivers.automaticNone
                        : auto(c.decision),
                }))}
                value={c.entry.driver}
                onChange={(k) =>
                  k != null && edit(c.ek, { driver: PrinterSchema.shape.driver.parse(k) })
                }
              />
              {c.decision && (
                <Disclosure title={strings.technicalDetails}>
                  <dl className="setup-facts">
                    <dt>{s.drivers.ruleLabel}</dt>
                    <dd>{strings.device.rules[c.decision.rule]}</dd>
                  </dl>
                </Disclosure>
              )}
            </Card>
          ) : (
            <Card key={c.ek} title={s.names.scannerTitle(c.entry.name)}>
              {c.decision && <p>{s.drivers.scannerExplained(family(c.decision.family))}</p>}
              <Disclosure title={strings.technicalDetails}>
                {c.decision && (
                  <dl className="setup-facts">
                    <dt>{s.drivers.ruleLabel}</dt>
                    <dd>{strings.device.scannerRules[c.decision.rule]}</dd>
                  </dl>
                )}
                <Select
                  label={s.drivers.backendLabel}
                  options={ScannerSchema.shape.backend.options.map((id) => ({
                    id,
                    label: id === "auto" ? auto(c.decision) : family(id),
                  }))}
                  value={c.entry.backend}
                  onChange={(k) =>
                    k != null && edit(c.ek, { backend: ScannerSchema.shape.backend.parse(k) })
                  }
                />
              </Disclosure>
            </Card>
          ),
        )
      )}
      <Card title={s.drivers.licencesTitle}>
        {eulas.length === 0 ? (
          <p>{s.drivers.noLicences}</p>
        ) : (
          eulas.map((e) => (
            <EulaAccept
              key={e.id}
              eula={e}
              accepted={acceptedSha(accepted, e.id, e.version)}
              onChange={(sha) => setAccepted(withAccepted(accepted, e.id, e.version, sha))}
            />
          ))
        )}
      </Card>
    </>
  );
}

function Review({
  setup,
  draft,
  all,
  chosen,
  title,
  accepted,
}: {
  setup: SetupDTO;
  draft: Draft;
  all: Row[];
  chosen: Chosen[];
  title: string | undefined;
  accepted: EULARef[];
}) {
  const shown = new Map(shownEulas(setup, chosen).map((e) => [eulaKey(e), e.title]));
  const licences = accepted
    .map((r) => shown.get(eulaKey(r)))
    .filter((t): t is string => t !== undefined);
  const describe = (c: Chosen) => {
    const where = c.entry.location ? ` (${c.entry.location})` : "";
    if (draft.simulated) return `${c.entry.name}${where} · ${s.review.simulatedDevice}`;
    const driver =
      c.role === "printer"
        ? c.entry.driver === "auto"
          ? s.drivers.automaticUnknown
          : family(c.entry.driver)
        : c.entry.backend === "auto"
          ? s.drivers.automaticUnknown
          : family(c.entry.backend);
    return `${c.entry.name}${where} · ${driver}`;
  };
  const list = (role: Role) => {
    const items = chosen.filter((c) => c.role === role);
    return items.length ? (
      <ul className="setup-review-list">
        {items.map((c) => (
          <li key={c.ek}>{describe(c)}</li>
        ))}
      </ul>
    ) : (
      s.review.none
    );
  };
  const declined = draft.simulated
    ? []
    : all.filter((r) => r.offered.some((role) => draft.use[`${r.key}:${role}`] === false));
  const noDriver = chosen.filter(
    (c) => c.role === "printer" && c.entry.driver === "auto" && c.decision?.rule === "none",
  );
  return (
    <Card>
      <dl className="setup-facts">
        {title !== undefined && (
          <>
            <dt>{s.review.title}</dt>
            <dd>{title}</dd>
          </>
        )}
        <dt>{s.review.printers}</dt>
        <dd>{list("printer")}</dd>
        <dt>{s.review.scanners}</dt>
        <dd>{list("scanner")}</dd>
        {declined.length > 0 && (
          <>
            <dt>{s.review.notUsed}</dt>
            <dd>{declined.map((r) => deviceName(r.d, false)).join(", ")}</dd>
          </>
        )}
        <dt>{s.review.licences}</dt>
        <dd>{licences.length ? licences.join(", ") : s.review.noLicences}</dd>
      </dl>
      {noDriver.map(
        (c) =>
          c.role === "printer" && (
            <NoDriver key={c.ek} device={c.decision?.device ?? null} hint={s.review.noDriverHint} />
          ),
      )}
    </Card>
  );
}

/** The no-driver state: what the printer reported, so the user can look up a driver for it. */
function NoDriver({ device, hint }: { device: USBDevice | null; hint?: string }) {
  const t = strings.device.noDriver;
  return (
    <div>
      <p>{t.title}</p>
      <p>
        {device?.deviceId ? (
          <>
            {t.deviceId} <code>{device.deviceId}</code>
          </>
        ) : (
          t.noDeviceId
        )}
      </p>
      {hint && <p>{hint}</p>}
    </div>
  );
}

function addedEntries(status: Status | null, added: Added) {
  const pick = <T extends { id: string }>(list: T[] | undefined, ids: string[]) =>
    ids.flatMap((id) => list?.find((e) => e.id === id) ?? []);
  return {
    printers: pick<StatusPrinter>(status?.printers, added.printers),
    scanners: pick<StatusScanner>(status?.scanners, added.scanners),
  };
}

function Applying({
  status,
  ids,
  timedOut,
}: {
  status: Status | null;
  ids: string[];
  timedOut: boolean;
}) {
  const entries = [...(status?.printers ?? []), ...(status?.scanners ?? [])].filter((e) =>
    ids.includes(e.id),
  );
  return (
    <div className="setup-view">
      <PageHeader title={s.applying.title} description={s.applying.description} />
      <Card>
        <ProgressBar label={s.applying.progress} />
        {entries.length > 0 && (
          <div role="status">
            <ul className="setup-devices">
              {entries.map((e) => (
                <li key={e.id}>
                  <span className="setup-device-name">
                    {e.name} <DeviceStatusBadge phase={e.phase} />
                  </span>
                  {e.message && <span className="setup-device-meta">{e.message}</span>}
                </li>
              ))}
            </ul>
          </div>
        )}
      </Card>
      {timedOut && (
        <InlineError action={<LinkButton href="/">{s.applying.goToStatus}</LinkButton>}>
          <p>{s.applying.timeout}</p>
        </InlineError>
      )}
    </div>
  );
}

type Changes = Record<string, OptionValue>;

/**
 * Defaults per new device from its live capabilities (A15), with the same control as Settings.
 * Save re-reads the config and patches only the edited arrays, changing only the edited keys.
 */
function DefaultsStep({
  status,
  added,
  title,
  onDone,
}: {
  status: Status | null;
  added: Added;
  title: string;
  onDone: () => void;
}) {
  const [changes, setChanges] = useState<Record<string, Changes>>({});
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<unknown>(null);
  const { printers, scanners } = addedEntries(status, added);
  const locale = mediaForLanguage(navigator.language);

  // What the step would write per entry: the user's choices over the locale's paper size,
  // which is preselected when the printer offers it and does not already default to it.
  const pending = new Map<string, Changes>();
  for (const p of printers) {
    const auto: Changes = {};
    const media = optionState("media", p.available ? p.capabilities : undefined, "printer");
    if (media.values.includes(locale) && media.deviceDefault !== locale) auto.media = locale;
    pending.set(`printer:${p.id}`, { ...auto, ...changes[`printer:${p.id}`] });
  }
  for (const sc of scanners) pending.set(`scanner:${sc.id}`, { ...changes[`scanner:${sc.id}`] });
  const hasChanges = [...pending.values()].some((c) =>
    Object.values(c).some((v) => v !== "" && v !== 0),
  );

  const set = (key: string, kind: FieldKind, value: OptionValue) =>
    setChanges((c) => ({ ...c, [key]: { ...c[key], [kind]: value } }));
  const applied = <D extends object>(role: Role, id: string, defaults: D): D | undefined => {
    const c = pending.get(`${role}:${id}`);
    if (!c || Object.keys(c).length === 0) return undefined;
    return Object.entries(c).reduce((d, [k, v]) => setDefault(d, k as FieldKind, v), defaults);
  };

  const save = async () => {
    if (!hasChanges) return onDone();
    if (saving) return;
    setSaving(true);
    setError(null);
    try {
      // The file as it is now: other entries and other keys stay exactly as they are.
      const now = await api.getConfig();
      if (!now.config) throw new Error(s.configInvalid);
      const patch: Record<string, unknown> = {};
      let p = false;
      const nextPrinters = now.config.printers.map((e) => {
        const d = applied("printer", e.id, e.defaults);
        if (!d) return e;
        p = true;
        return { ...e, defaults: d };
      });
      let sc = false;
      const nextScanners = now.config.scanners.map((e) => {
        const d = applied("scanner", e.id, e.defaults);
        if (!d) return e;
        sc = true;
        return { ...e, defaults: d };
      });
      if (p) patch.printers = nextPrinters;
      if (sc) patch.scanners = nextScanners;
      await api.patchConfig(patch, now.etag);
      onDone();
    } catch (err) {
      setError(err);
    } finally {
      setSaving(false);
    }
  };

  const card = (role: Role, e: StatusPrinter | StatusScanner) => {
    const key = `${role}:${e.id}`;
    const caps = e.available ? e.capabilities : undefined;
    const groups = role === "printer" ? printerGroups : scannerGroups;
    return (
      <Card key={key} title={e.name}>
        {groups.map((g) => (
          <section key={g.id} className="setup-option-group" aria-label={g.title}>
            <h3>{g.title}</h3>
            {g.kinds.map((kind: FieldKind) => (
              <OptionField
                key={kind}
                kind={kind}
                device={role}
                mode="defaults"
                state={optionState(kind, caps, role)}
                value={pending.get(key)?.[kind]}
                onChange={(v) => set(key, kind, v)}
              />
            ))}
          </section>
        ))}
      </Card>
    );
  };

  return (
    <div className="setup-view">
      <Progress current="defaults" title={title} />
      <PageHeader
        title={s.steps.defaults}
        description={s.defaults.description}
        documentTitle={s.stepTitle(PROGRESS.length, PROGRESS.length, s.steps.defaults)}
      />
      {printers.map((e) => card("printer", e))}
      {scanners.map((e) => card("scanner", e))}
      {error !== null && (
        <InlineError
          action={
            error instanceof ConflictError && <Button onPress={save}>{s.defaults.saveAgain}</Button>
          }
        >
          <p>{error instanceof ConflictError ? s.defaults.conflict : errorText(error)}</p>
        </InlineError>
      )}
      <div className="setup-actions">
        <Button variant="quiet" onPress={onDone}>
          {s.defaults.skip}
        </Button>
        <Button variant="primary" onPress={save}>
          {saving ? s.defaults.saving : hasChanges ? s.defaults.save : s.defaults.keep}
        </Button>
      </div>
    </div>
  );
}

function Done({ status, added }: { status: Status | null; added: Added }) {
  const { printers, scanners } = addedEntries(status, added);
  const connected = [
    ...printers.map((p) => ({
      id: p.id,
      name: p.name,
      dnssd: p.connect.dnssdName,
      ipp: p.connect.ippUris,
      escl: [] as string[],
    })),
    ...scanners.map((sc) => ({
      id: sc.id,
      name: sc.name,
      dnssd: sc.connect.dnssdName,
      ipp: [] as string[],
      escl: sc.connect.esclUrls,
    })),
  ].filter((c) => c.dnssd || c.ipp.length > 0 || c.escl.length > 0);
  return (
    <div className="setup-view">
      <PageHeader title={s.done.title} description={s.done.description} />
      {connected.length > 0 && <p>{s.done.connectHint}</p>}
      {connected.map((c) => (
        <Card key={c.id} title={c.name}>
          <dl className="setup-facts">
            {c.dnssd && (
              <>
                <dt>{s.done.networkName}</dt>
                <dd>{c.dnssd}</dd>
              </>
            )}
            {c.ipp.map((u) => (
              <Fact key={u} label={s.done.printerAddress} value={u} />
            ))}
            {c.escl.map((u) => (
              <Fact key={u} label={s.done.scannerAddress} value={u} />
            ))}
          </dl>
        </Card>
      ))}
      <div className="setup-actions">
        <LinkButton href="/" variant="primary">
          {s.done.goToStatus}
        </LinkButton>
      </div>
    </div>
  );
}

function Fact({ label, value }: { label: string; value: string }) {
  return (
    <>
      <dt>{label}</dt>
      <dd>
        <code>{value}</code>
      </dd>
    </>
  );
}
