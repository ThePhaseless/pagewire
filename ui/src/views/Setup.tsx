// The setup wizard: five steps that build the first config from setup.suggested, then follow
// Status until the device is ready. The current step lives in the URL hash.
import { useEffect, useState } from "react";
import { api, ConflictError, ProblemError, problemMessage } from "../api/client";
import { useStatus } from "../api/events";
import { acceptedSha, EulaAccept, withAccepted } from "../components/EulaAccept";
import {
  Button,
  Card,
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
  DeviceSchema,
  type Setup as SetupDTO,
  type Status,
} from "../gen/api";
import { navigate } from "../router";
import { strings } from "../strings";
import { setupStrings as s } from "./Setup.strings";
import "./Setup.css";

const STEPS = ["detect", "name", "driver", "defaults", "review"] as const;
type Step = (typeof STEPS)[number];

const A4 = "iso_a4_210x297mm";
const LETTER = "na_letter_8.5x11in";
const RESOLUTIONS = [75, 150, 300, 600, 1200];

/** The paper size people in this locale expect: Letter in the US and Canada, A4 elsewhere. */
export function mediaForLanguage(lang: string): string {
  return /^en-(us|ca)$/i.test(lang) ? LETTER : A4;
}

function fromSuggested(c: Config): Config {
  const media = mediaForLanguage(navigator.language);
  return { ...c, print: { ...c.print, defaults: { ...c.print.defaults, media } } };
}

function stepFromHash(): Step {
  const h = window.location.hash.slice(1);
  return (STEPS as readonly string[]).includes(h) ? (h as Step) : "detect";
}

const family = (id: string) => s.families[id] ?? id;
const options = (labels: Record<string, string>) =>
  Object.entries(labels).map(([id, label]) => ({ id, label }));

// Module level, so the reload timer is not restarted by every render.
const reloadApp = () => window.location.assign("/");

export type SetupProps = {
  /** How long to wait for the device to become ready before saying so. */
  readyTimeoutMs?: number;
  /** Called after a 412 (another session created the config first). */
  reload?: () => void;
  reloadDelayMs?: number;
};

export function Setup({
  readyTimeoutMs = 180_000,
  reload = reloadApp,
  reloadDelayMs = 3000,
}: SetupProps = {}) {
  const status = useStatus();
  const [setup, setSetup] = useState<SetupDTO | null>(null);
  const [loadError, setLoadError] = useState<unknown>(null);
  const [attempt, setAttempt] = useState(0);
  // The user's draft. Until the first edit it follows the latest setup.suggested.
  const [edited, setEdited] = useState<Config | null>(null);
  const [step, setStep] = useState<Step>("detect");
  const [tried, setTried] = useState(false);
  const [flow, setFlow] = useState<"form" | "applying" | "done">("form");
  const [submitting, setSubmitting] = useState(false);
  const [submitError, setSubmitError] = useState<unknown>(null);
  const [conflict, setConflict] = useState(false);
  const [timedOut, setTimedOut] = useState(false);

  // The draft is not kept across reloads, so a reload starts again at Detect.
  useEffect(() => {
    if (window.location.hash)
      window.history.replaceState(null, "", window.location.pathname + window.location.search);
    const onPop = () => setStep(stepFromHash());
    window.addEventListener("popstate", onPop);
    return () => window.removeEventListener("popstate", onPop);
  }, []);

  // GET /setup has no event of its own: refetch whenever the probe's Status changes.
  const inv = status?.device.inventory;
  const refreshKey = [
    inv?.probedAt,
    inv?.devices.length,
    status?.device.phase,
    status?.device.message,
  ].join("|");
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

  useEffect(() => {
    if (
      flow === "applying" &&
      status?.config.state === "applied" &&
      status.device.phase === "ready"
    )
      setFlow("done");
  }, [flow, status]);

  useEffect(() => {
    if (flow !== "applying") return;
    const t = setTimeout(() => setTimedOut(true), readyTimeoutMs);
    return () => clearTimeout(t);
  }, [flow, readyTimeoutMs]);

  useEffect(() => {
    if (!conflict) return;
    const t = setTimeout(reload, reloadDelayMs);
    return () => clearTimeout(t);
  }, [conflict, reload, reloadDelayMs]);

  // Applying and Done replace the pressed button, so move focus to their heading.
  useEffect(() => {
    if (flow !== "form") document.querySelector<HTMLElement>(".setup-view h1")?.focus();
  }, [flow]);

  if (flow === "applying") return <Applying status={status} timedOut={timedOut} />;
  if (flow === "done") return <Done status={status} />;

  if (!setup) {
    return (
      <div className="setup-view">
        <PageHeader title={strings.nav.setup} />
        {loadError ? (
          <InlineError
            action={
              <Button onPress={() => setAttempt((a) => a + 1)}>{strings.buttons.retry}</Button>
            }
          >
            <p>{s.loadFailed}</p>
            <p>{problemMessage(loadError)}</p>
          </InlineError>
        ) : (
          <p role="status">{strings.loading}</p>
        )}
      </div>
    );
  }

  if (!setup.required && !conflict) {
    return (
      <div className="setup-view">
        <PageHeader
          title={s.alreadyDone.title}
          description={s.alreadyDone.body}
          actions={
            <LinkButton href="/settings" variant="primary">
              {s.alreadyDone.action}
            </LinkButton>
          }
        />
      </div>
    );
  }

  const draft = edited ?? fromSuggested(setup.suggested);
  const update = (fn: (c: Config) => Config) => setEdited(fn(draft));
  const idx = STEPS.indexOf(step);
  const go = (to: Step) => {
    setTried(false);
    setStep(to);
    navigate(`#${to}`);
  };

  const nameOk = ConfigSchema.shape.name.safeParse(draft.name).success;
  const locationOk = ConfigSchema.shape.location.safeParse(draft.location).success;
  const chosenSimulated = edited?.device.mode === "simulated";
  const canContinue =
    step !== "detect" ||
    (draft.device.mode === "simulated" ? chosenSimulated : setup.decision !== null);

  const next = () => {
    if (step === "name" && !(nameOk && locationOk)) {
      setTried(true);
      // After the error messages render, take keyboard users to the first one.
      requestAnimationFrame(() =>
        document.querySelector<HTMLElement>(".setup-view [aria-invalid='true']")?.focus(),
      );
      return;
    }
    setEdited(draft);
    const to = STEPS[idx + 1];
    if (to) go(to);
  };

  const chooseSimulated = () => {
    setEdited({ ...draft, device: { mode: "simulated", driver: "auto", scanner: "auto" } });
    go("name");
  };

  const finish = async () => {
    if (submitting) return;
    // Only licences the wizard showed (and therefore asked about) are written.
    const shown = new Set(setup.eulas.map((e) => `${e.id}@${e.version}`));
    const body: Config = {
      ...draft,
      licenses: {
        acceptedEulas: draft.licenses.acceptedEulas.filter((r) =>
          shown.has(`${r.id}@${r.version}`),
        ),
      },
    };
    setSubmitting(true);
    setSubmitError(null);
    try {
      await api.createConfig(body);
      setFlow("applying");
    } catch (err) {
      if (err instanceof ConflictError) setConflict(true);
      else setSubmitError(err);
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <div className="setup-view">
      <p className="setup-eyebrow">
        {s.title} · {s.stepOf(idx + 1, STEPS.length)}
      </p>
      <ol className="setup-steps" aria-label={s.progressLabel}>
        {STEPS.map((st, i) => (
          <li
            key={st}
            aria-current={st === step ? "step" : undefined}
            data-done={i < idx || undefined}
          >
            <span className="setup-step-number tabular" aria-hidden="true">
              {i + 1}
            </span>
            <span className="setup-step-label">{s.steps[st]}</span>
          </li>
        ))}
      </ol>
      <PageHeader
        title={s.steps[step]}
        description={descriptions[step]}
        documentTitle={s.stepTitle(idx + 1, STEPS.length, s.steps[step])}
      />

      {step === "detect" && (
        <Detect
          setup={setup}
          status={status}
          chosenSimulated={chosenSimulated}
          onSimulated={chooseSimulated}
          onUsb={() => update((d) => ({ ...d, device: setup.suggested.device }))}
        />
      )}
      {step === "name" && (
        <Card>
          <TextField
            label={s.name.nameLabel}
            description={s.name.nameHint}
            value={draft.name}
            onChange={(name) => update((d) => ({ ...d, name }))}
            errorMessage={tried && !nameOk ? s.name.nameInvalid : undefined}
            isRequired
          />
          <TextField
            label={s.name.locationLabel}
            description={s.name.locationHint}
            value={draft.location}
            onChange={(location) => update((d) => ({ ...d, location }))}
            errorMessage={tried && !locationOk ? s.name.locationInvalid : undefined}
          />
        </Card>
      )}
      {step === "driver" && <Driver setup={setup} draft={draft} update={update} />}
      {step === "defaults" && <Defaults draft={draft} update={update} />}
      {step === "review" && <Review setup={setup} draft={draft} />}

      {conflict && (
        <InlineError action={<Button onPress={reload}>{s.review.reloadNow}</Button>}>
          <p>{s.review.conflict}</p>
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
            <p>{problemMessage(submitError)}</p>
          )}
        </InlineError>
      )}

      <div className="setup-actions">
        {idx > 0 && (
          <Button onPress={() => go(STEPS[idx - 1] ?? "detect")}>{strings.buttons.back}</Button>
        )}
        {step === "review" ? (
          // No isPending: the button unmounts on success, and React Aria's pending announcement
          // would be left pointing at nothing.
          <Button variant="primary" onPress={finish} isDisabled={conflict}>
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
  detect: s.detect.description,
  name: s.name.description,
  driver: s.driver.description,
  defaults: s.defaults.description,
  review: s.review.description,
};

function Detect({
  setup,
  status,
  chosenSimulated,
  onSimulated,
  onUsb,
}: {
  setup: SetupDTO;
  status: Status | null;
  chosenSimulated: boolean;
  onSimulated: () => void;
  onUsb: () => void;
}) {
  const message = status?.device.message ?? "";
  const simButton = (primary: boolean) => (
    <Button variant={primary ? "primary" : "secondary"} onPress={onSimulated}>
      {s.detect.useSimulated}
    </Button>
  );

  if (chosenSimulated) {
    return (
      <Card title={s.detect.simulatedTitle}>
        <p>{s.detect.simulatedBody}</p>
        {setup.usbResourceConfigured && (
          <div>
            <Button variant="quiet" onPress={onUsb}>
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

  const inventory = status?.device.inventory ?? setup.inventory;
  if (!inventory) {
    return (
      <Card title={s.detect.searchingTitle}>
        <p role="status">{message || s.detect.waiting}</p>
        <p>{s.detect.plugInHint}</p>
        <div>{simButton(false)}</div>
      </Card>
    );
  }

  const d = setup.decision;
  return (
    <>
      <Card title={s.detect.devicesTitle}>
        {inventory.devices.length === 0 ? (
          <p>{s.detect.noDevices}</p>
        ) : (
          <ul className="setup-devices">
            {inventory.devices.map((dev) => (
              <li key={`${dev.bus}-${dev.address}`}>
                <span className="setup-device-name">
                  {inventory.simulated
                    ? s.detect.simulatedName(dev.product || s.detect.unnamedDevice)
                    : dev.product || s.detect.unnamedDevice}
                </span>
                <span className="setup-device-meta">
                  {[
                    dev.manufacturer,
                    s.detect.usbIds(dev.vendorId, dev.productId),
                    dev.serial && s.detect.serial(dev.serial),
                  ]
                    .filter(Boolean)
                    .join(" · ")}
                </span>
              </li>
            ))}
          </ul>
        )}
      </Card>
      {d ? (
        <Card title={s.detect.choiceTitle}>
          <dl className="setup-facts">
            <dt>{s.detect.printerLabel}</dt>
            <dd>{family(d.printer)}</dd>
            <dt>{s.detect.scannerLabel}</dt>
            <dd>{family(d.scanner || "none")}</dd>
          </dl>
          <Disclosure title={strings.technicalDetails}>
            <dl className="setup-facts">
              <dt>{s.detect.ruleLabel}</dt>
              <dd>{d.rule}</dd>
            </dl>
          </Disclosure>
        </Card>
      ) : (
        <Card title={s.detect.searchingTitle}>
          <p role="status">{message || s.detect.noPrinter}</p>
          <p>{s.detect.plugInHint}</p>
          <div>{simButton(false)}</div>
        </Card>
      )}
    </>
  );
}

type Update = (fn: (c: Config) => Config) => void;

function Driver({ setup, draft, update }: { setup: SetupDTO; draft: Config; update: Update }) {
  const d = setup.decision;
  const simulated = draft.device.mode === "simulated";
  const auto = (id: string | undefined) =>
    id === undefined ? s.driver.automaticUnknown : s.driver.automatic(family(id));
  const setAccepted = (id: string, version: string, sha: string | null) =>
    update((c) => ({
      ...c,
      licenses: { acceptedEulas: withAccepted(c.licenses.acceptedEulas, id, version, sha) },
    }));

  return (
    <>
      <Card>
        {simulated ? (
          <p>{s.driver.simulated}</p>
        ) : (
          <>
            {d && <p>{s.driver.explained(family(d.printer), family(d.scanner || "none"))}</p>}
            <Select
              label={s.driver.driverLabel}
              options={DeviceSchema.shape.driver.options.map((id) => ({
                id,
                label: id === "auto" ? auto(d?.printer) : family(id),
              }))}
              value={draft.device.driver}
              onChange={(k) =>
                k != null &&
                update((c) => ({
                  ...c,
                  device: { ...c.device, driver: DeviceSchema.shape.driver.parse(k) },
                }))
              }
            />
            <Select
              label={s.driver.scannerLabel}
              options={DeviceSchema.shape.scanner.options.map((id) => ({
                id,
                label: id === "auto" ? auto(d ? d.scanner || "none" : undefined) : family(id),
              }))}
              value={draft.device.scanner}
              onChange={(k) =>
                k != null &&
                update((c) => ({
                  ...c,
                  device: { ...c.device, scanner: DeviceSchema.shape.scanner.parse(k) },
                }))
              }
            />
          </>
        )}
      </Card>
      <Card title={s.driver.licencesTitle}>
        {setup.eulas.length === 0 ? (
          <p>{s.driver.noLicences}</p>
        ) : (
          setup.eulas.map((e) => (
            <EulaAccept
              key={e.id}
              eula={e}
              accepted={acceptedSha(draft.licenses.acceptedEulas, e.id, e.version)}
              onChange={(sha) => setAccepted(e.id, e.version, sha)}
            />
          ))
        )}
      </Card>
    </>
  );
}

function Defaults({ draft, update }: { draft: Config; update: Update }) {
  const p = draft.print.defaults;
  const sc = draft.scan.defaults;
  const setPrint = (patch: Partial<Config["print"]["defaults"]>) =>
    update((c) => ({ ...c, print: { ...c.print, defaults: { ...c.print.defaults, ...patch } } }));
  const setScan = (patch: Partial<Config["scan"]["defaults"]>) =>
    update((c) => ({ ...c, scan: { ...c.scan, defaults: { ...c.scan.defaults, ...patch } } }));
  const media = options(s.defaults.mediaOptions);
  if (!media.some((m) => m.id === p.media)) media.push({ id: p.media, label: p.media });
  const S = ConfigSchema.shape;

  return (
    <>
      <Card title={s.defaults.printTitle}>
        <Select
          label={s.defaults.media}
          options={media}
          value={p.media}
          onChange={(k) => k != null && setPrint({ media: String(k) })}
        />
        <RadioGroup
          label={s.defaults.sides}
          options={options(s.defaults.sidesOptions)}
          value={p.sides}
          onChange={(v) => setPrint({ sides: S.print.shape.defaults.shape.sides.parse(v) })}
        />
      </Card>
      <Card title={s.defaults.scanTitle}>
        <RadioGroup
          label={s.defaults.source}
          options={options(s.defaults.sourceOptions)}
          value={sc.source}
          onChange={(v) => setScan({ source: S.scan.shape.defaults.shape.source.parse(v) })}
        />
        <Select
          label={s.defaults.resolution}
          options={RESOLUTIONS.map((n) => ({ id: String(n), label: s.defaults.dpi(n) }))}
          value={String(sc.resolution)}
          onChange={(k) => k != null && setScan({ resolution: Number(k) })}
        />
        <RadioGroup
          label={s.defaults.colorMode}
          options={options(s.defaults.colorOptions)}
          value={sc.colorMode}
          onChange={(v) => setScan({ colorMode: S.scan.shape.defaults.shape.colorMode.parse(v) })}
        />
        <RadioGroup
          label={s.defaults.format}
          options={options(s.defaults.formatOptions)}
          value={sc.format}
          onChange={(v) => setScan({ format: S.scan.shape.defaults.shape.format.parse(v) })}
        />
      </Card>
    </>
  );
}

function Review({ setup, draft }: { setup: SetupDTO; draft: Config }) {
  const simulated = draft.device.mode === "simulated";
  const titles = new Map(setup.eulas.map((e) => [`${e.id}@${e.version}`, e.title]));
  const licences = draft.licenses.acceptedEulas
    .map((r) => titles.get(`${r.id}@${r.version}`))
    .filter((t): t is string => t !== undefined);
  const p = draft.print.defaults;
  const sc = draft.scan.defaults;
  const d = s.defaults;
  return (
    <Card>
      <dl className="setup-facts">
        <dt>{s.review.name}</dt>
        <dd>{draft.name}</dd>
        <dt>{s.review.location}</dt>
        <dd>{draft.location || s.review.noLocation}</dd>
        <dt>{s.review.device}</dt>
        <dd>{simulated ? s.review.simulatedDevice : s.review.usbDevice}</dd>
        {!simulated && (
          <>
            <dt>{s.review.driver}</dt>
            <dd>
              {draft.device.driver === "auto"
                ? s.driver.automaticUnknown
                : family(draft.device.driver)}
            </dd>
            <dt>{s.review.scanner}</dt>
            <dd>
              {draft.device.scanner === "auto"
                ? s.driver.automaticUnknown
                : family(draft.device.scanner)}
            </dd>
          </>
        )}
        <dt>{s.review.licences}</dt>
        <dd>{licences.length ? licences.join(", ") : s.review.noLicences}</dd>
        <dt>{s.review.printDefaults}</dt>
        <dd>{[d.mediaOptions[p.media] ?? p.media, d.sidesOptions[p.sides]].join(", ")}</dd>
        <dt>{s.review.scanDefaults}</dt>
        <dd>
          {[
            d.sourceOptions[sc.source],
            d.dpi(sc.resolution),
            d.colorOptions[sc.colorMode],
            d.formatOptions[sc.format],
          ].join(", ")}
        </dd>
      </dl>
    </Card>
  );
}

function Applying({ status, timedOut }: { status: Status | null; timedOut: boolean }) {
  const message = status?.device.message;
  return (
    <div className="setup-view">
      <PageHeader title={s.applying.title} description={s.applying.description} />
      <Card>
        <ProgressBar label={s.applying.progress} />
        {message && <p role="status">{message}</p>}
      </Card>
      {timedOut && (
        <InlineError action={<LinkButton href="/">{s.applying.goToStatus}</LinkButton>}>
          <p>{s.applying.timeout}</p>
        </InlineError>
      )}
    </div>
  );
}

function Done({ status }: { status: Status | null }) {
  const c = status?.connect;
  return (
    <div className="setup-view">
      <PageHeader title={s.done.title} description={s.done.description} />
      {c && (c.dnssdName || c.ippUris.length > 0 || c.esclUrls.length > 0) && (
        <Card>
          <p>{s.done.connectHint}</p>
          <dl className="setup-facts">
            {c.dnssdName && (
              <>
                <dt>{s.done.networkName}</dt>
                <dd>{c.dnssdName}</dd>
              </>
            )}
            {c.ippUris.map((u) => (
              <Fact key={u} label={s.done.printerAddress} value={u} />
            ))}
            {c.esclUrls.map((u) => (
              <Fact key={u} label={s.done.scannerAddress} value={u} />
            ))}
          </dl>
        </Card>
      )}
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
