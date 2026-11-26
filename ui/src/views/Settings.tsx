// The settings editor: one form over the config file, saved as an RFC 7396 merge patch with
// If-Match. A 412 opens ConflictDialog; an invalid file shows its errors and a way back.
// Printers and scanners are arrays, so any edit inside one sends the whole list (A1, section 0).
import { type ReactNode, useCallback, useContext, useEffect, useId, useRef, useState } from "react";
import { api, ConflictError, ProblemError, problemMessage } from "../api/client";
import { EventsContext, useStatus } from "../api/events";
import { type ConflictChange, ConflictDialog } from "../components/ConflictDialog";
import { acceptedSha, EulaAccept, withAccepted } from "../components/EulaAccept";
import { OptionField } from "../components/OptionField";
import {
  Button,
  Card,
  DeviceStatusBadge,
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
  type FieldError,
  PrinterSchema,
  ScannerSchema,
  type Setup as SetupDTO,
  type Status,
  type USB,
  type USBDevice,
} from "../gen/api";
import { optionTitle } from "../lib/labels";
import { changedPaths, diff, valueAt } from "../lib/mergepatch";
import {
  type FieldKind,
  type OptionValue,
  optionState,
  printerGroups,
  printKinds,
  pruneUnsupported,
  scanKinds,
  scannerGroups,
  setDefault,
} from "../lib/options";
import { type Blocker, useNavigationBlocker } from "../router";
import { strings } from "../strings";
import { settingsStrings as s } from "./Settings.strings";
import "./Settings.css";

const f = s.fields;

type Obj = Record<string, unknown>;
type Role = "printer" | "scanner";
type Printer = Config["printers"][number];
type Scanner = Config["scanners"][number];
type Entry = Printer | Scanner;
/** A change to one entry; the keys only one role has are left out for the other. */
type EntryPatch = Partial<Omit<Printer & Scanner, "defaults">> & { defaults?: Entry["defaults"] };
const LIST = { printer: "printers", scanner: "scanners" } as const;

const familyOptions = (ids: readonly string[]) =>
  ids.map((id) => ({ id, label: id === "auto" ? f.automatic : (f.families[id] ?? id) }));

/** How one list item reads in the conflict dialog: an entry by name, a device by its USB IDs. */
function itemLabel(r: unknown): string {
  if (typeof r !== "object" || r === null) return String(r);
  const o = r as Obj;
  if (typeof o.name === "string") return o.enabled === false ? s.off(o.name) : o.name;
  if (typeof o.vendorId === "string") return `${o.vendorId}:${o.productId}`;
  return `${o.id} ${o.version}`;
}

function display(v: unknown): string {
  if (v === undefined || v === null || v === "") return s.notSet;
  if (Array.isArray(v)) return v.length ? v.map(itemLabel).join(", ") : s.none;
  if (typeof v === "object") return Object.values(v).filter(Boolean).join(" · ");
  return String(v);
}

const ENTRY_PATH = /^\/(printers|scanners)\/(\d+)\/(.+)$/;

/** The field label of a pointer inside one printer or scanner, or undefined when it has none. */
function entryLabel(path: string, doc: Config | null): string | undefined {
  const m = ENTRY_PATH.exec(path);
  if (!m) return undefined;
  const [, list = "", i = "", rest = ""] = m;
  const role: Role = list === "printers" ? "printer" : "scanner";
  const kind = rest.startsWith("defaults/") ? rest.slice("defaults/".length) : undefined;
  const kinds: readonly string[] = role === "printer" ? printKinds : scanKinds;
  const field = kind
    ? kinds.includes(kind)
      ? optionTitle(kind as FieldKind, role)
      : undefined
    : s.entryPaths[rest];
  if (!field) return undefined;
  const entries: Entry[] | undefined = doc?.[list as "printers" | "scanners"];
  return s.entryPath(entries?.[Number(i)]?.name || s.unnamed, field);
}

const pathLabel = (path: string, doc: Config | null) =>
  s.paths[path] ?? entryLabel(path, doc) ?? path;

const GENERAL = new Set(["/name", "/print/maxUploadMiB", "/scan/retentionDays"]);
/** Pointers that have a place in the form; any other error goes to the summary. */
const placed = (path: string) =>
  GENERAL.has(path) || path.startsWith("/licenses/") || entryLabel(path, null) !== undefined;

/** `c` with entry `i` of `role` changed by `patch` (undefined values drop the key on save). */
function withEntry(c: Config, role: Role, i: number, patch: EntryPatch): Config {
  return role === "printer"
    ? { ...c, printers: c.printers.map((e, j) => (j === i ? ({ ...e, ...patch } as Printer) : e)) }
    : { ...c, scanners: c.scanners.map((e, j) => (j === i ? ({ ...e, ...patch } as Scanner) : e)) };
}

function withoutEntry(c: Config, role: Role, i: number): Config {
  return role === "printer"
    ? { ...c, printers: c.printers.filter((_, j) => j !== i) }
    : { ...c, scanners: c.scanners.filter((_, j) => j !== i) };
}

const entryKey = (role: Role, id: string) => `${role}:${id}`;

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
  const [open, setOpen] = useState<ReadonlySet<string>>(new Set());
  const [removing, setRemoving] = useState<{ role: Role; index: number } | null>(null);

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

  /** The device's capabilities, or undefined while it is not connected (A23 "not connected"). */
  const capsOf = (role: Role, id: string) => {
    const x = (role === "printer" ? status?.printers : status?.scanners)?.find((d) => d.id === id);
    return x?.available ? x.capabilities : undefined;
  };

  /**
   * What Save writes: each entry the user changed loses the defaults its connected device does
   * not offer (U18), which clears their Status warnings. Untouched entries go out as they are.
   */
  const forSave = (c: Config): Config => {
    const prune = <E extends Entry>(role: Role, list: E[], before: E[]) =>
      list.map((e) => {
        const b = before.find((x) => x.id === e.id);
        if (b && JSON.stringify(b) === JSON.stringify(e)) return e;
        return { ...e, defaults: pruneUnsupported(e.defaults, capsOf(role, e.id), role) };
      });
    return base
      ? {
          ...c,
          printers: prune("printer", c.printers, base.printers),
          scanners: prune("scanner", c.scanners, base.scanners),
        }
      : c;
  };

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
      if (err instanceof ProblemError && err.status === 422) {
        setErrors(err.errors);
        // Open every entry that holds an error, so the error is on screen.
        setOpen((prev) => {
          const next = new Set(prev);
          for (const e of err.errors) {
            const m = ENTRY_PATH.exec(e.path);
            const list = m?.[1] as "printers" | "scanners" | undefined;
            const entry: Entry | undefined = list && draft?.[list][Number(m?.[2])];
            if (entry) next.add(entryKey(list === "printers" ? "printer" : "scanner", entry.id));
          }
          return next;
        });
      } else setSaveError(err);
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
        label: pathLabel(path, draft),
        mine: display(valueAt(draft, path)),
        file: display(valueAt(conflict.current.config, path)),
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
            label: pathLabel(path, conflict.current.config),
            mine: display(valueAt(draft, path)),
            file: display(valueAt(conflict.current.config, path)),
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
        void write(() => api.replaceConfig(forSave(draft), current.etag), p);
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
  const errorAt = (path: string, prefix = false) =>
    errors
      .filter((e) => e.path === path || (prefix && e.path.startsWith(`${path}/`)))
      .map((e) => e.message)
      .join(" ") || undefined;
  /** Highlight class and server error for the field at `path`. */
  const at = (path: string) => ({
    className: marked.has(path) ? "settings-highlight" : undefined,
    errorMessage: errorAt(path),
  });
  const unplaced = errors.filter((e) => !placed(e.path));
  const fileChanged = dirty && changedEtag !== null && changedEtag !== resp.etag;
  const toggle = (key: string) =>
    setOpen((prev) => {
      const next = new Set(prev);
      if (!next.delete(key)) next.add(key);
      return next;
    });

  const reset = () => {
    setEdited(null);
    setErrors([]);
    setHighlight([]);
    setSaveError(null);
    if (changedEtag !== null && changedEtag !== resp.etag) void load();
  };

  const entries = (role: Role) => {
    const list: Entry[] = draft[LIST[role]];
    return list.length === 0 ? (
      <p className="settings-muted">{role === "printer" ? s.noPrinters : s.noScanners}</p>
    ) : (
      list.map((e, i) => (
        <EntryEditor
          key={e.id}
          role={role}
          entry={e}
          index={i}
          status={status}
          caps={capsOf(role, e.id)}
          isOpen={open.has(entryKey(role, e.id))}
          onToggle={() => toggle(entryKey(role, e.id))}
          onRemove={() => setRemoving({ role, index: i })}
          set={(p) => update((c) => withEntry(c, role, i, p))}
          at={at}
          errorAt={errorAt}
        />
      ))
    );
  };
  const removingEntry: Entry | undefined = removing
    ? draft[LIST[removing.role]][removing.index]
    : undefined;

  return (
    <div className="settings-view">
      <PageHeader
        title={strings.nav.settings}
        description={s.description}
        actions={<LinkButton href="/setup?add=1">{s.detect}</LinkButton>}
      />

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
          label={f.title}
          description={f.titleHint}
          value={draft.name}
          onChange={(name) => update((c) => ({ ...c, name }))}
          isRequired
          {...at("/name")}
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
      </Card>

      <Card
        title={s.sections.printers}
        className={marked.has("/printers") ? "settings-highlight" : undefined}
      >
        {entries("printer")}
      </Card>

      <Card
        title={s.sections.scanners}
        className={marked.has("/scanners") ? "settings-highlight" : undefined}
      >
        {entries("scanner")}
      </Card>

      <Card
        title={s.sections.ignored}
        className={marked.has("/ignoredDevices") ? "settings-highlight" : undefined}
      >
        <Ignored draft={draft} status={status} update={update} />
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
          onPress={() => {
            const sent = diff(base, forSave(draft));
            void write(() => api.patchConfig(sent, resp.etag), sent);
          }}
        >
          {strings.buttons.save}
        </Button>
      </div>
      {dialog}
      <Dialog
        role="alertdialog"
        isOpen={removing !== null}
        onOpenChange={(isOpen) => !isOpen && setRemoving(null)}
        title={s.remove.title(removingEntry?.name || s.unnamed)}
      >
        {(close) => (
          <>
            <p>{s.remove.body}</p>
            <div className="settings-dialog-actions">
              <Button onPress={close}>{strings.buttons.cancel}</Button>
              <Button
                variant="danger"
                onPress={() => {
                  const r = removing;
                  close();
                  if (r) update((c) => withoutEntry(c, r.role, r.index));
                }}
              >
                {s.remove.confirm}
              </Button>
            </div>
          </>
        )}
      </Dialog>
      <Dialog
        role="alertdialog"
        isOpen={leave !== null}
        onOpenChange={(isOpen) => !isOpen && setLeave(null)}
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

const ANY = "any";
const usbId = (u: USB | undefined) => (u ? `${u.vendorId}:${u.productId}:${u.serial}` : ANY);

/** "Brother HL-L2350DW (E7)"; the USB IDs when the device reports no names. */
function deviceName(d: Pick<USBDevice, "vendorId" | "productId" | "serial"> & Partial<USBDevice>) {
  const mfg = d.manufacturer ?? "";
  const product = d.product ?? "";
  const name =
    (product.toLowerCase().startsWith(mfg.toLowerCase()) ? product : `${mfg} ${product}`).trim() ||
    `${d.vendorId}:${d.productId}`;
  return d.serial ? `${name} (${d.serial})` : name;
}

function EntryEditor({
  role,
  entry,
  index,
  status,
  caps,
  isOpen,
  onToggle,
  onRemove,
  set,
  at,
  errorAt,
}: {
  role: Role;
  entry: Entry;
  index: number;
  status: Status | null;
  caps: Parameters<typeof optionState>[1];
  isOpen: boolean;
  onToggle: () => void;
  onRemove: () => void;
  set: (patch: EntryPatch) => void;
  at: At;
  errorAt: (path: string, prefix?: boolean) => string | undefined;
}) {
  const headingId = useId();
  const panelId = useId();
  const path = `/${LIST[role]}/${index}`;
  const phase = (role === "printer" ? status?.printers : status?.scanners)?.find(
    (d) => d.id === entry.id,
  )?.phase;
  const printer = role === "printer" ? (entry as Printer) : undefined;
  const defaults = entry.defaults as Record<string, OptionValue | undefined>;

  // The detected devices that can take this role, plus the saved one when it is not plugged in.
  const detected = (status?.devices ?? []).filter((d) =>
    role === "printer" ? d.canPrint : d.canScan,
  );
  const usbOptions = new Map([[ANY, f.anyDevice]]);
  for (const d of detected) {
    const id = usbId(d.device);
    if (!usbOptions.has(id)) usbOptions.set(id, deviceName(d.device));
  }
  // The device the selector binds to (A2: an empty serial matches any), the bound one first.
  const u = entry.usb;
  const matches = (d: USBDevice) =>
    u !== undefined &&
    d.vendorId === u.vendorId &&
    d.productId === u.productId &&
    (u.serial === "" || d.serial === u.serial);
  const bound =
    detected.find(
      (d) => (role === "printer" ? d.printerId : d.scannerId) === entry.id && matches(d.device),
    ) ?? detected.find((d) => matches(d.device));
  const current = bound ? usbId(bound.device) : usbId(u);
  if (u && !usbOptions.has(current)) usbOptions.set(current, f.notDetected(deviceName(u)));

  return (
    <section className="settings-entry" aria-labelledby={headingId}>
      <header className="settings-entry-head">
        <div className="settings-entry-title">
          <h3 id={headingId}>{entry.name || s.unnamed}</h3>
          {entry.location && <p className="settings-muted">{entry.location}</p>}
        </div>
        {phase && <DeviceStatusBadge phase={phase} />}
        <div className="settings-entry-actions">
          <Switch isSelected={entry.enabled} onChange={(enabled) => set({ enabled })}>
            {f.enabled}
          </Switch>
          <Button
            variant="quiet"
            aria-expanded={isOpen}
            aria-controls={isOpen ? panelId : undefined}
            onPress={onToggle}
          >
            {isOpen ? s.done : s.edit}
          </Button>
          <Button variant="quiet" onPress={onRemove}>
            {s.remove.action}
          </Button>
        </div>
      </header>
      {isOpen && (
        <div id={panelId} className="settings-entry-body">
          <TextField
            label={f.name}
            description={f.nameHint(role)}
            value={entry.name}
            onChange={(name) => set({ name })}
            isRequired
            {...at(`${path}/name`)}
          />
          <TextField
            label={f.location}
            description={f.locationHint}
            value={entry.location}
            onChange={(location) => set({ location })}
            {...at(`${path}/location`)}
          />
          {/* Simulated devices are for tests and demos: only an entry that already is one can switch to USB. */}
          {entry.mode === "simulated" && (
            <RadioGroup
              label={f.mode}
              options={PrinterSchema.shape.mode.options.map((id) => ({
                id,
                label: f.modeOptions(role)[id] ?? id,
              }))}
              value={entry.mode}
              onChange={(v) => set({ mode: PrinterSchema.shape.mode.parse(v) })}
              {...at(`${path}/mode`)}
            />
          )}
          {entry.mode === "usb" && (
            <Select
              label={f.usb}
              description={f.usbHint(role)}
              options={[...usbOptions].map(([id, label]) => ({ id, label }))}
              selectedKey={current}
              onSelectionChange={(k) => {
                // The bound device is already what the selector means; keep the file as it is.
                if (k === current) return;
                if (k === ANY) return set({ usb: undefined });
                const d = detected.find((x) => usbId(x.device) === k)?.device;
                if (d)
                  set({ usb: { vendorId: d.vendorId, productId: d.productId, serial: d.serial } });
              }}
              errorMessage={errorAt(`${path}/usb`, true)}
            />
          )}
          {printer ? (
            <>
              <Select
                label={f.driver}
                options={familyOptions(PrinterSchema.shape.driver.options)}
                selectedKey={printer.driver}
                onSelectionChange={(k) => {
                  if (k == null) return;
                  const driver = PrinterSchema.shape.driver.parse(k);
                  // ipp-usb needs no driver, so a driver name is not allowed with it.
                  set(driver === "ipp-usb" ? { driver, driverName: undefined } : { driver });
                }}
                {...at(`${path}/driver`)}
              />
              {printer.mode === "usb" && printer.driver !== "ipp-usb" && (
                <TextField
                  label={f.driverName}
                  description={f.driverNameHint}
                  value={printer.driverName ?? ""}
                  // Empty removes the key, so the printer goes back to automatic choice.
                  onChange={(v) => set({ driverName: v || undefined })}
                  {...at(`${path}/driverName`)}
                />
              )}
            </>
          ) : (
            <Select
              label={f.backend}
              options={familyOptions(ScannerSchema.shape.backend.options)}
              selectedKey={(entry as Scanner).backend}
              onSelectionChange={(k) =>
                k != null && set({ backend: ScannerSchema.shape.backend.parse(k) })
              }
              {...at(`${path}/backend`)}
            />
          )}
          {(role === "printer" ? printerGroups : scannerGroups).map((g) => (
            <OptionGroup key={g.id} title={g.title}>
              {g.kinds.map((kind: FieldKind) => (
                <OptionField
                  key={kind}
                  kind={kind}
                  device={role}
                  mode="defaults"
                  state={optionState(kind, caps, role)}
                  value={defaults[kind]}
                  onChange={(v) => set({ defaults: setDefault(entry.defaults, kind, v) })}
                  errorMessage={errorAt(`${path}/defaults/${kind}`)}
                />
              ))}
            </OptionGroup>
          ))}
        </div>
      )}
    </section>
  );
}

function OptionGroup({ title, children }: { title: string; children: ReactNode }) {
  return (
    <div className="settings-option-group">
      <h4>{title}</h4>
      {children}
    </div>
  );
}

function Ignored({
  draft,
  status,
  update,
}: {
  draft: Config;
  status: Status | null;
  update: Update;
}) {
  if (draft.ignoredDevices.length === 0) return <p className="settings-muted">{s.noIgnored}</p>;
  return (
    <>
      <p className="settings-muted">{s.ignoredHint}</p>
      <ul className="settings-ignored">
        {draft.ignoredDevices.map((d, i) => {
          const seen = status?.devices.find(
            (x) =>
              x.device.vendorId === d.vendorId &&
              x.device.productId === d.productId &&
              x.device.serial === d.serial,
          )?.device;
          return (
            <li key={`${d.vendorId}:${d.productId}:${d.serial}`} className="settings-ignored-item">
              <span className="settings-ignored-name">{deviceName(seen ?? d)}</span>
              <span className="settings-muted">{s.ignoredRoles(d.roles)}</span>
              <Button
                variant="quiet"
                onPress={() =>
                  update((c) => ({
                    ...c,
                    ignoredDevices: c.ignoredDevices.filter((_, j) => j !== i),
                  }))
                }
              >
                {s.offerAgain}
              </Button>
            </li>
          );
        })}
      </ul>
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
  const blobs = status?.workloads.flatMap((w) => w.blobs) ?? [];
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
        const blob = blobs.find((b) => b.id === e.id && b.version === e.version);
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
