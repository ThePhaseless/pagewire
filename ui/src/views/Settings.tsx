// The settings editor: one form over the config file, saved as an RFC 7396 merge patch with
// If-Match. A 412 opens ConflictDialog; an invalid file shows its errors and a way back.
import { useCallback, useContext, useEffect, useRef, useState } from "react";
import { api, ConflictError, ProblemError, problemMessage } from "../api/client";
import { EventsContext, useStatus } from "../api/events";
import { type ConflictChange, ConflictDialog } from "../components/ConflictDialog";
import { acceptedSha, EulaAccept, withAccepted } from "../components/EulaAccept";
import {
  Button,
  Card,
  Dialog,
  EmptyState,
  InlineError,
  LinkButton,
  NumberField,
  PageHeader,
  RadioGroup,
  Select,
  Switch,
  TextField,
  toast,
} from "../components/ui";
import {
  type Config,
  type ConfigResponse,
  ConfigSchema,
  type FieldError,
  type Setup as SetupDTO,
  type Status,
} from "../gen/api";
import { changedPaths, diff, valueAt } from "../lib/mergepatch";
import { type Blocker, useNavigationBlocker } from "../router";
import { strings } from "../strings";
import { settingsStrings as s } from "./Settings.strings";
import "./Settings.css";

const f = s.fields;
const S = ConfigSchema.shape;
const RESOLUTIONS = [75, 150, 300, 600, 1200];

const options = (labels: Record<string, string>, ids: readonly string[] = Object.keys(labels)) =>
  ids.map((id) => ({ id, label: labels[id] ?? id }));
const familyOptions = (ids: readonly string[]) =>
  ids.map((id) => ({ id, label: id === "auto" ? f.automatic : (f.families[id] ?? id) }));

/** Option labels per pointer, for showing values in the conflict dialog and review note. */
const valueLabels: Record<string, Record<string, string>> = {
  "/device/mode": f.modeOptions,
  "/device/driver": { auto: f.automatic, ...f.families },
  "/device/scanner": { auto: f.automatic, ...f.families },
  "/print/defaults/media": f.mediaOptions,
  "/print/defaults/sides": f.sidesOptions,
  "/print/defaults/colorMode": f.printColorOptions,
  "/print/defaults/quality": f.qualityOptions,
  "/scan/defaults/source": f.sourceOptions,
  "/scan/defaults/colorMode": f.scanColorOptions,
  "/scan/defaults/format": f.formatOptions,
};

function display(path: string, v: unknown): string {
  if (v === undefined || v === null || v === "") return s.notSet;
  if (Array.isArray(v))
    return v.length
      ? v.map((r) => (typeof r === "object" ? `${r.id} ${r.version}` : String(r))).join(", ")
      : s.none;
  if (typeof v === "object") return Object.values(v).filter(Boolean).join(" · ");
  return valueLabels[path]?.[String(v)] ?? String(v);
}

/** Pointers that have a place in the form; any other error goes to the summary. */
const placed = (path: string) => path in s.paths || path.startsWith("/licenses/");

type Obj = Record<string, unknown>;

export function Settings() {
  const store = useContext(EventsContext);
  const status = useStatus();
  const [resp, setResp] = useState<ConfigResponse | null>(null);
  const [loadError, setLoadError] = useState<unknown>(null);
  const [setup, setSetup] = useState<SetupDTO | null>(null);
  const [edited, setEdited] = useState<Config | null>(null);
  const [errors, setErrors] = useState<FieldError[]>([]);
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState<unknown>(null);
  const [conflict, setConflict] = useState<{ current: ConfigResponse; patch: Obj } | null>(null);
  const [highlight, setHighlight] = useState<ConflictChange[]>([]);
  const [changedEtag, setChangedEtag] = useState<string | null>(null);
  const [confirmRestore, setConfirmRestore] = useState(false);

  // Read by load() and the SSE handler without resubscribing on every render.
  const live = useRef<{ dirty: boolean; etag?: string }>({ dirty: false });

  const load = useCallback(async () => {
    try {
      const r = await api.getConfig();
      // An edit started while the GET was in flight is based on the old file: keep its etag so
      // Save still surfaces the conflict, and only say that the file changed.
      if (live.current.dirty) setChangedEtag(r.etag);
      else {
        setResp(r);
        setChangedEtag(null);
      }
      setLoadError(null);
    } catch (err) {
      setLoadError(err);
    }
  }, []);

  useEffect(() => {
    void load();
    // The licence list comes from setup; without it the Licences section is just empty.
    api.getSetup().then(setSetup, () => {});
  }, [load]);

  const base = resp?.state === "applied" ? resp.config : null;
  const draft = edited ?? base;
  const patch = base && draft ? diff(base, draft) : {};
  const dirty = Object.keys(patch).length > 0;

  live.current = { dirty, etag: resp?.etag };

  // Unsaved edits: ask before another page replaces the form, and before the tab unloads.
  const [leave, setLeave] = useState<(() => void) | null>(null);
  const block = useCallback<Blocker>((proceed) => {
    setLeave(() => proceed);
    return true;
  }, []);
  useNavigationBlocker(dirty ? block : null);
  useEffect(() => {
    if (!dirty) return;
    const onUnload = (e: BeforeUnloadEvent) => e.preventDefault();
    window.addEventListener("beforeunload", onUnload);
    return () => window.removeEventListener("beforeunload", onUnload);
  }, [dirty]);
  useEffect(
    () =>
      store.on("config", (e) => {
        // Our own save echoes back with the etag we already hold.
        if (e.etag === live.current.etag) return;
        // While editing, only say so: the stale etag makes Save surface the conflict.
        if (live.current.dirty) setChangedEtag(e.etag);
        else void load();
      }),
    [store, load],
  );

  /** Replaces the form with the file `r` describes. */
  const show = (r: ConfigResponse) => {
    setResp(r);
    setChangedEtag(null);
    setEdited(null);
    setErrors([]);
    setSaveError(null);
    setConflict(null);
    setHighlight([]);
  };

  /** Sends a write. `sentPatch` is the form edit a 412 should offer to resolve; null shows the file. */
  const write = async (send: () => Promise<ConfigResponse>, sentPatch: Obj | null) => {
    setSaving(true);
    setSaveError(null);
    try {
      show(await send());
      toast({ title: s.saved, tone: "ok" });
      // A new device or mode can change which licences apply.
      api.getSetup().then(setSetup, () => {});
    } catch (err) {
      if (err instanceof ConflictError) {
        if (sentPatch) setConflict({ current: err.current, patch: sentPatch });
        else show(err.current);
        return;
      }
      setConflict(null);
      if (err instanceof ProblemError && err.status === 422) setErrors(err.errors);
      else setSaveError(err);
    } finally {
      setSaving(false);
    }
  };

  if (!resp) {
    const missing = loadError instanceof ProblemError && loadError.status === 404;
    return (
      <div className="settings-view">
        <PageHeader title={strings.nav.settings} />
        {missing ? (
          <EmptyState
            icon="settings"
            title={s.missing.title}
            action={
              <LinkButton href="/setup" variant="primary">
                {s.missing.action}
              </LinkButton>
            }
          >
            <p>{s.missing.body}</p>
          </EmptyState>
        ) : loadError ? (
          <InlineError action={<Button onPress={load}>{strings.buttons.retry}</Button>}>
            <p>{s.loadFailed}</p>
            <p>{problemMessage(loadError)}</p>
          </InlineError>
        ) : (
          <p role="status">{strings.loading}</p>
        )}
      </div>
    );
  }

  const changes: ConflictChange[] = conflict
    ? changedPaths(conflict.patch).map((path) => ({
        path,
        label: s.paths[path] ?? path,
        mine: display(path, valueAt(draft, path)),
        file: display(path, valueAt(conflict.current.config, path)),
      }))
    : [];
  // What the other edit changed that you did not touch: saving keeps those values.
  const mine = new Set(changes.map((c) => c.path));
  const alsoChanged: ConflictChange[] =
    conflict && base && conflict.current.config
      ? changedPaths(diff(base, conflict.current.config))
          .filter((path) => !mine.has(path))
          .map((path) => ({
            path,
            label: s.paths[path] ?? path,
            mine: display(path, valueAt(draft, path)),
            file: display(path, valueAt(conflict.current.config, path)),
          }))
      : [];

  const dialog = conflict && draft && (
    <ConflictDialog
      current={conflict.current}
      changes={changes}
      alsoChanged={alsoChanged}
      busy={saving}
      onKeepMine={() => {
        const { current, patch: p } = conflict;
        void write(() => api.patchConfig(p, current.etag), p);
      }}
      onUseFile={() => show(conflict.current)}
      onReview={() => {
        show(conflict.current);
        setHighlight(changes);
      }}
      onReplace={() => {
        const { current, patch: p } = conflict;
        void write(() => api.replaceConfig(draft, current.etag), p);
      }}
      onCancel={() =>
        conflict.current.state === "invalid" ? show(conflict.current) : setConflict(null)
      }
    />
  );

  if (!base || !draft) {
    return (
      <div className="settings-view">
        <PageHeader title={strings.nav.settings} />
        <InvalidFile resp={resp} onRestore={() => setConfirmRestore(true)} saveError={saveError} />
        {resp.lastGood && (
          <Dialog
            role="alertdialog"
            isOpen={confirmRestore}
            onOpenChange={setConfirmRestore}
            title={s.invalid.restoreTitle}
          >
            {(close) => (
              <>
                <p>{s.invalid.restoreBody}</p>
                <div className="settings-dialog-actions">
                  <Button onPress={close}>{strings.buttons.cancel}</Button>
                  <Button
                    variant="danger"
                    onPress={() => {
                      const { lastGood, etag } = resp;
                      close();
                      if (lastGood) void write(() => api.replaceConfig(lastGood, etag), null);
                    }}
                  >
                    {s.invalid.restoreConfirm}
                  </Button>
                </div>
              </>
            )}
          </Dialog>
        )}
        {dialog}
      </div>
    );
  }

  const update = (fn: (c: Config) => Config) => setEdited(fn(draft));
  const marked = new Set(highlight.map((h) => h.path));
  const errorAt = (path: string) =>
    errors
      .filter((e) => e.path === path)
      .map((e) => e.message)
      .join(" ") || undefined;
  /** Highlight class and server error for the field at `path`. */
  const at = (path: string) => ({
    className: marked.has(path) ? "settings-highlight" : undefined,
    errorMessage: errorAt(path),
  });
  const unplaced = errors.filter((e) => !placed(e.path));
  const fileChanged = dirty && changedEtag !== null && changedEtag !== resp.etag;

  const reset = () => {
    setEdited(null);
    setErrors([]);
    setHighlight([]);
    setSaveError(null);
    if (changedEtag !== null && changedEtag !== resp.etag) void load();
  };

  return (
    <div className="settings-view">
      <PageHeader title={strings.nav.settings} description={s.description} />

      {highlight.length > 0 && (
        <div className="settings-banner settings-review" role="status">
          <p>{s.reviewNote}</p>
          <ul>
            {highlight.map((h) => (
              <li key={h.path}>{s.errorAt(h.label, h.mine)}</li>
            ))}
          </ul>
        </div>
      )}

      <Card title={s.sections.general}>
        <TextField
          label={f.name}
          description={f.nameHint}
          value={draft.name}
          onChange={(name) => update((c) => ({ ...c, name }))}
          isRequired
          {...at("/name")}
        />
        <TextField
          label={f.location}
          description={f.locationHint}
          value={draft.location}
          onChange={(location) => update((c) => ({ ...c, location }))}
          {...at("/location")}
        />
      </Card>

      <Device draft={draft} status={status} update={update} at={at} errorAt={errorAt} />

      <Card title={s.sections.printing}>
        <PrintFields draft={draft} status={status} update={update} at={at} />
      </Card>

      <Card title={s.sections.scanning}>
        <ScanFields draft={draft} update={update} at={at} />
      </Card>

      <Card
        title={s.sections.licences}
        className={marked.has("/licenses/acceptedEulas") ? "settings-highlight" : undefined}
      >
        <Licences
          draft={draft}
          setup={setup}
          status={status}
          update={update}
          errors={errors.filter((e) => e.path.startsWith("/licenses/"))}
        />
      </Card>

      {(unplaced.length > 0 || saveError !== null) && (
        <InlineError>
          {saveError !== null && <p>{problemMessage(saveError)}</p>}
          {unplaced.length > 0 && (
            <>
              <p>{s.invalidValues}</p>
              <ul>
                {unplaced.map((e) => (
                  <li key={e.path + e.message}>{s.errorAt(e.path || "/", e.message)}</li>
                ))}
              </ul>
            </>
          )}
        </InlineError>
      )}

      <div className="settings-actions" data-sticky={dirty || undefined}>
        <div className="settings-actions-status" role="status">
          {dirty && <p className="settings-unsaved">{s.unsaved}</p>}
          {fileChanged && <p>{s.fileChanged}</p>}
        </div>
        <Button onPress={reset} isDisabled={!dirty && !fileChanged && highlight.length === 0}>
          {s.reset}
        </Button>
        <Button
          variant="primary"
          isDisabled={!dirty}
          isPending={saving}
          onPress={() => void write(() => api.patchConfig(patch, resp.etag), patch)}
        >
          {strings.buttons.save}
        </Button>
      </div>
      {dialog}
      <Dialog
        role="alertdialog"
        isOpen={leave !== null}
        onOpenChange={(open) => !open && setLeave(null)}
        title={s.leave.title}
      >
        {(close) => (
          <>
            <p>{s.leave.body}</p>
            <div className="settings-dialog-actions">
              <Button onPress={close}>{s.leave.stay}</Button>
              <Button
                variant="danger"
                onPress={() => {
                  const go = leave;
                  close();
                  go?.();
                }}
              >
                {s.leave.confirm}
              </Button>
            </div>
          </>
        )}
      </Dialog>
    </div>
  );
}

type Update = (fn: (c: Config) => Config) => void;
type At = (path: string) => { className?: string; errorMessage?: string };

function Device({
  draft,
  status,
  update,
  at,
  errorAt,
}: {
  draft: Config;
  status: Status | null;
  update: Update;
  at: At;
  errorAt: (path: string) => string | undefined;
}) {
  const dev = draft.device;
  const setDevice = (patch: Partial<Config["device"]>) =>
    update((c) => ({ ...c, device: { ...c.device, ...patch } }));
  const usb = dev.usb;
  const setUsb = (patch: Partial<NonNullable<Config["device"]["usb"]>>) =>
    usb && setDevice({ usb: { ...usb, ...patch } });
  const usbError = errorAt("/device/usb");

  return (
    <Card title={s.sections.device}>
      <RadioGroup
        label={f.mode}
        options={options(f.modeOptions, S.device.shape.mode.options)}
        value={dev.mode}
        onChange={(v) => {
          const mode = S.device.shape.mode.parse(v);
          // A USB selector and a driver name are only allowed with mode usb.
          setDevice(
            mode === "simulated" ? { mode, usb: undefined, driverName: undefined } : { mode },
          );
        }}
        {...at("/device/mode")}
      />
      <Select
        label={f.driver}
        options={familyOptions(S.device.shape.driver.options)}
        value={dev.driver}
        onChange={(k) => {
          if (k == null) return;
          const driver = S.device.shape.driver.parse(k);
          // ipp-usb needs no driver, so a driver name is not allowed with it.
          setDevice(driver === "ipp-usb" ? { driver, driverName: undefined } : { driver });
        }}
        {...at("/device/driver")}
      />
      {dev.mode === "usb" && dev.driver !== "ipp-usb" && (
        <TextField
          label={f.driverName}
          description={f.driverNameHint}
          value={dev.driverName ?? ""}
          // Empty removes the key, so the file goes back to automatic choice.
          onChange={(v) => setDevice({ driverName: v || undefined })}
          {...at("/device/driverName")}
        />
      )}
      <Select
        label={f.scanner}
        options={familyOptions(S.device.shape.scanner.options)}
        value={dev.scanner}
        onChange={(k) => k != null && setDevice({ scanner: S.device.shape.scanner.parse(k) })}
        {...at("/device/scanner")}
      />
      {dev.mode === "usb" && (
        <div className="settings-usb">
          <Switch
            isSelected={usb !== undefined}
            onChange={(on) => {
              const m = status?.device.decision?.device;
              setDevice({
                usb: on
                  ? {
                      vendorId: m?.vendorId ?? "",
                      productId: m?.productId ?? "",
                      serial: m?.serial ?? "",
                    }
                  : undefined,
              });
            }}
            className={at("/device/usb").className}
          >
            {f.usbSelector}
          </Switch>
          <p className="ui-field-description">{f.usbSelectorHint}</p>
          {usbError && <InlineError>{usbError}</InlineError>}
          {usb && (
            <div className="settings-usb-fields">
              <TextField
                label={f.vendorId}
                description={f.usbIdHint}
                value={usb.vendorId}
                onChange={(vendorId) => setUsb({ vendorId })}
                {...at("/device/usb/vendorId")}
              />
              <TextField
                label={f.productId}
                description={f.usbIdHint}
                value={usb.productId}
                onChange={(productId) => setUsb({ productId })}
                {...at("/device/usb/productId")}
              />
              <TextField
                label={f.serial}
                description={f.serialHint}
                value={usb.serial}
                onChange={(serial) => setUsb({ serial })}
                {...at("/device/usb/serial")}
              />
            </div>
          )}
        </div>
      )}
    </Card>
  );
}

function PrintFields({
  draft,
  status,
  update,
  at,
}: {
  draft: Config;
  status: Status | null;
  update: Update;
  at: At;
}) {
  const p = draft.print.defaults;
  const D = S.print.shape.defaults.shape;
  const setDefaults = (patch: Partial<Config["print"]["defaults"]>) =>
    update((c) => ({ ...c, print: { ...c.print, defaults: { ...c.print.defaults, ...patch } } }));
  const media = [
    ...new Set([...Object.keys(f.mediaOptions), ...(status?.printer.media ?? []), p.media]),
  ];
  return (
    <>
      <Select
        label={f.media}
        options={options(f.mediaOptions, media)}
        value={p.media}
        onChange={(k) => k != null && setDefaults({ media: String(k) })}
        {...at("/print/defaults/media")}
      />
      <RadioGroup
        label={f.sides}
        options={options(f.sidesOptions, D.sides.options)}
        value={p.sides}
        onChange={(v) => setDefaults({ sides: D.sides.parse(v) })}
        {...at("/print/defaults/sides")}
      />
      <RadioGroup
        label={f.printColor}
        options={options(f.printColorOptions, D.colorMode.options)}
        value={p.colorMode}
        onChange={(v) => setDefaults({ colorMode: D.colorMode.parse(v) })}
        {...at("/print/defaults/colorMode")}
      />
      <RadioGroup
        label={f.quality}
        options={options(f.qualityOptions, D.quality.options)}
        value={p.quality}
        onChange={(v) => setDefaults({ quality: D.quality.parse(v) })}
        {...at("/print/defaults/quality")}
      />
      <NumberField
        label={f.maxUpload}
        description={f.maxUploadHint}
        minValue={1}
        maxValue={500}
        formatOptions={{ maximumFractionDigits: 0 }}
        value={draft.print.maxUploadMiB}
        // A cleared field gives NaN, which would serialise as null and delete the key.
        onChange={(n) =>
          Number.isFinite(n) && update((c) => ({ ...c, print: { ...c.print, maxUploadMiB: n } }))
        }
        {...at("/print/maxUploadMiB")}
      />
    </>
  );
}

function ScanFields({ draft, update, at }: { draft: Config; update: Update; at: At }) {
  const sc = draft.scan.defaults;
  const D = S.scan.shape.defaults.shape;
  const setDefaults = (patch: Partial<Config["scan"]["defaults"]>) =>
    update((c) => ({ ...c, scan: { ...c.scan, defaults: { ...c.scan.defaults, ...patch } } }));
  const resolutions = [...new Set([...RESOLUTIONS, sc.resolution])].sort((a, b) => a - b);
  return (
    <>
      <RadioGroup
        label={f.source}
        options={options(f.sourceOptions, D.source.options)}
        value={sc.source}
        onChange={(v) => setDefaults({ source: D.source.parse(v) })}
        {...at("/scan/defaults/source")}
      />
      <Select
        label={f.resolution}
        options={resolutions.map((n) => ({ id: String(n), label: f.dpi(n) }))}
        value={String(sc.resolution)}
        onChange={(k) => k != null && setDefaults({ resolution: Number(k) })}
        {...at("/scan/defaults/resolution")}
      />
      <RadioGroup
        label={f.scanColor}
        options={options(f.scanColorOptions, D.colorMode.options)}
        value={sc.colorMode}
        onChange={(v) => setDefaults({ colorMode: D.colorMode.parse(v) })}
        {...at("/scan/defaults/colorMode")}
      />
      <RadioGroup
        label={f.format}
        options={options(f.formatOptions, D.format.options)}
        value={sc.format}
        onChange={(v) => setDefaults({ format: D.format.parse(v) })}
        {...at("/scan/defaults/format")}
      />
      <NumberField
        label={f.retention}
        description={f.retentionHint}
        minValue={0}
        maxValue={3650}
        formatOptions={{ maximumFractionDigits: 0 }}
        value={draft.scan.retentionDays}
        onChange={(n) =>
          Number.isFinite(n) && update((c) => ({ ...c, scan: { ...c.scan, retentionDays: n } }))
        }
        {...at("/scan/retentionDays")}
      />
    </>
  );
}

function Licences({
  draft,
  setup,
  status,
  update,
  errors,
}: {
  draft: Config;
  setup: SetupDTO | null;
  status: Status | null;
  update: Update;
  errors: FieldError[];
}) {
  const eulas = setup?.eulas ?? [];
  const setAccepted = (id: string, version: string, sha: string | null) =>
    update((c) => ({
      ...c,
      licenses: { acceptedEulas: withAccepted(c.licenses.acceptedEulas, id, version, sha) },
    }));
  return (
    <>
      {eulas.length === 0 && <p>{s.noLicences}</p>}
      {eulas.map((e) => {
        // shortcut: Status blobs carry no EULA id; the catalog names each blob after its EULA,
        // so match on id and version. Upgrade when a blob and its EULA can differ.
        const blob = status?.device.blobs.find((b) => b.id === e.id && b.version === e.version);
        return (
          <div key={`${e.id}@${e.version}`} className="settings-eula">
            <EulaAccept
              eula={e}
              accepted={acceptedSha(draft.licenses.acceptedEulas, e.id, e.version)}
              onChange={(sha) => setAccepted(e.id, e.version, sha)}
            />
            {blob && (
              <p className="settings-blob" data-state={blob.state}>
                <span className="settings-blob-label">{s.blobLabel}</span>{" "}
                {s.blobState(s.blob[blob.state] ?? blob.state, blob.message)}
              </p>
            )}
          </div>
        );
      })}
      {errors.length > 0 && (
        <InlineError>
          <ul>
            {errors.map((e) => (
              <li key={e.path + e.message}>{e.message}</li>
            ))}
          </ul>
        </InlineError>
      )}
    </>
  );
}

function InvalidFile({
  resp,
  onRestore,
  saveError,
}: {
  resp: ConfigResponse;
  onRestore: () => void;
  saveError: unknown;
}) {
  return (
    <Card title={s.invalid.title} tone="problem">
      <p>{resp.lastGood ? s.invalid.withLastGood : s.invalid.withoutLastGood}</p>
      <ul className="settings-file-errors">
        {resp.errors.map((e) => (
          <li key={`${e.line}:${e.column}:${e.path}:${e.message}`}>
            {s.errorAt(s.invalid.location(e.line, e.column), e.message)}
          </li>
        ))}
      </ul>
      <div className="settings-invalid-actions">
        <LinkButton href={api.rawConfigUrl} download>
          {s.invalid.download}
        </LinkButton>
        {resp.lastGood && (
          <Button variant="primary" onPress={onRestore}>
            {s.invalid.restore}
          </Button>
        )}
      </div>
      {saveError !== null && (
        <InlineError>
          <p>{s.invalid.restoreFailed}</p>
          <p>{problemMessage(saveError)}</p>
        </InlineError>
      )}
    </Card>
  );
}
