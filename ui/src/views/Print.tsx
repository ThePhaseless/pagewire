// The Print page: pick or drop documents, check them against the printer's formats, choose
// options, upload with progress, and follow and cancel jobs.
import { useEffect, useRef, useState } from "react";
import { Text } from "react-aria-components";
import { api, problemMessage, uploadPrint } from "../api/client";
import { useConnected, useEnabledPrinters, useNothingFound, useStatus } from "../api/events";
import { DevicePicker } from "../components/DevicePicker";
import { OptionField } from "../components/OptionField";
import {
  Button,
  Card,
  Disclosure,
  DropZone,
  EmptyState,
  FileTrigger,
  Icon,
  InlineError,
  LinkButton,
  NumberField,
  PageHeader,
  ProgressBar,
  StatusBadge,
  type Tone,
  toast,
} from "../components/ui";
import type { Config, Job, PrinterCaps, PrintOptions } from "../gen/api";
import { useDeviceChoice } from "../lib/devices";
import { optionLabel, warningText } from "../lib/labels";
import { useJobsWithArrivals } from "../lib/motion";
import {
  copiesLimit,
  type OptionGroup,
  optionState,
  type PrintKind,
  printerGroups,
  toRequest,
} from "../lib/options";
import { strings } from "../strings";
import { printStrings as t } from "./Print.strings";
import "./Print.css";

// Mirrors config.Default: the upload limit while the config is not loaded.
const fallbackMaxUploadMiB = 50;

// Paper and Printing stay open; Layout and Output sit under "More options".
const MORE = new Set(["layout", "output"]);

// Mirrors internal/printing.Detect: the server decides by content, never by name or file.type.
const UNKNOWN = "application/octet-stream";
const magics: [number[], string][] = [
  [[0x25, 0x50, 0x44, 0x46, 0x2d], "application/pdf"], // %PDF-
  [[0x25, 0x21], "application/postscript"], // %!
  [[0xff, 0xd8, 0xff], "image/jpeg"],
  [[0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a], "image/png"],
  [[0x52, 0x61, 0x53, 0x32], "image/pwg-raster"], // RaS2
  [[0x55, 0x4e, 0x49, 0x52, 0x41, 0x53, 0x54], "image/urf"], // UNIRAST
];

/** UTF-8 without control characters other than tab, LF, FF and CR; a rune cut off at the end is fine. */
function isText(head: Uint8Array): boolean {
  let s: string;
  try {
    // stream: true leaves an incomplete trailing sequence pending instead of failing on it.
    s = new TextDecoder("utf-8", { fatal: true, ignoreBOM: true }).decode(head, { stream: true });
  } catch {
    return false;
  }
  for (const ch of s) {
    const c = ch.codePointAt(0) ?? 0;
    if ((c < 0x20 && c !== 0x09 && c !== 0x0a && c !== 0x0c && c !== 0x0d) || c === 0x7f) {
      return false;
    }
  }
  return true;
}

/** The document format of a file from its first 512 bytes, the same way the server detects it. */
export function detectFormat(head: Uint8Array): string {
  for (const [sig, format] of magics) {
    if (head.length >= sig.length && sig.every((b, i) => head[i] === b)) return format;
  }
  return head.length > 0 && isText(head) ? "text/plain" : UNKNOWN;
}

const baseType = (mime: string) => (mime.split(";")[0] ?? "").trim().toLowerCase();

/** Mirrors internal/printing.Negotiate: an unknown format is refused even if the printer lists octet-stream. */
export function formatAccepted(detected: string, supported: string[]): boolean {
  return detected !== UNKNOWN && supported.some((s) => baseType(s) === detected);
}

const formatName = (mime: string) => t.formats[mime] ?? mime;

const OFFICE = /\.(docx?|odt|rtf|pages|xlsx?|ods|numbers|pptx?|odp|key)$/i;
const PHOTO = /\.(heic|heif|webp|avif|gif|tiff?|bmp|dng|raw|png|jpe?g)$/i;

/** How to turn a refused file into a printable one, judged by its name and detected format. */
export function helpFor(name: string, detected: string | null | undefined): string | null {
  if (OFFICE.test(name)) return t.help.document;
  if (PHOTO.test(name) || detected?.startsWith("image/")) return t.help.photo;
  if (detected === "text/plain") return t.help.text;
  return null;
}

const jobTone: Record<Job["state"], Tone> = {
  pending: "busy",
  processing: "active",
  completed: "ok",
  canceled: "idle",
  aborted: "problem",
  held: "idle",
  stopped: "problem",
};

const cancellable = new Set<Job["state"]>(["pending", "processing", "held", "stopped"]);

const dateFormat = new Intl.DateTimeFormat(undefined, { dateStyle: "medium", timeStyle: "short" });

/** A formatted local time, or null for the zero time (or anything unparsable). */
function formatTime(iso: string): string | null {
  const ms = Date.parse(iso);
  return ms > 0 ? dateFormat.format(ms) : null;
}
const listFormat = new Intl.ListFormat("en", { type: "conjunction" });

type Item = {
  key: number;
  file: File;
  /** The detected format; undefined while the head is being read, null when it could not be read. */
  format: string | null | undefined;
  /** Upload progress 0..1 while this file uploads, else null. */
  progress: number | null;
  error: string | null;
};

/** Why this file cannot be printed, or null when it can (or is still being checked). */
function problemFor(item: Item, formats: string[], maxUploadMiB: number): string | null {
  if (item.format === null) return t.files.readError;
  if (item.format !== undefined && !formatAccepted(item.format, formats)) {
    return item.format === UNKNOWN
      ? t.files.unknownFormat
      : t.files.unsupported(formatName(item.format));
  }
  if (item.file.size > maxUploadMiB * 1024 * 1024) return t.files.tooLarge(maxUploadMiB);
  return null;
}

/** The effective config's print settings (the file's, else its last good version), refreshed on every file change. */
function usePrintConfig(etag: string | undefined, state: string | undefined) {
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

function JobList({ printerId, available }: { printerId: string | undefined; available: boolean }) {
  const { jobs, error, arrived } = useJobsWithArrivals(printerId);
  const [canceling, setCanceling] = useState<Set<number>>(new Set());

  async function cancel(job: Job) {
    setCanceling((s) => new Set(s).add(job.id));
    try {
      await api.cancelJob(job.printer, job.id);
    } catch (err) {
      toast({
        title: t.jobs.cancelFailed(job.name),
        description: problemMessage(err),
        tone: "problem",
      });
    } finally {
      setCanceling((s) => {
        const next = new Set(s);
        next.delete(job.id);
        return next;
      });
    }
  }

  return (
    <Card title={t.jobs.title} className="print-jobs">
      {available && error ? <InlineError>{problemMessage(error)}</InlineError> : null}
      {jobs.length > 0 ? (
        <ul className="print-list">
          {jobs.map((j) => (
            <li key={j.id} className="print-row" data-arrived={arrived(j.id) || undefined}>
              <span className="print-row-main">
                <span className="print-name">{j.name}</span>
                <span className="print-subtle tabular">{formatTime(j.createdAt)}</span>
              </span>
              <StatusBadge tone={jobTone[j.state]} pulse={j.state === "processing"}>
                {t.jobs.state[j.state]}
              </StatusBadge>
              {cancellable.has(j.state) && (
                <Button
                  variant="quiet"
                  aria-label={t.jobs.cancelJob(j.name)}
                  isDisabled={canceling.has(j.id)}
                  onPress={() => void cancel(j)}
                >
                  {t.jobs.cancel}
                </Button>
              )}
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

/** One option group's shown controls, or nothing when the printer lets you change none of them. */
function Group({
  group,
  caps,
  chosen,
  set,
}: {
  group: OptionGroup<PrintKind>;
  caps: PrinterCaps | undefined;
  chosen: PrintOptions;
  set: (patch: Partial<PrintOptions>) => void;
}) {
  const kinds = group.kinds.filter((k) => optionState(k, caps, "printer").reason === null);
  if (kinds.length === 0) return null;
  return (
    <section className="print-group">
      <h3 className="print-group-title">{group.title}</h3>
      {kinds.map((kind) => (
        <OptionField
          key={kind}
          kind={kind}
          device="printer"
          mode="job"
          state={optionState(kind, caps, "printer")}
          value={chosen[kind]}
          onChange={(v) => set({ [kind]: v })}
        />
      ))}
    </section>
  );
}

const shows = (g: OptionGroup<PrintKind>, caps: PrinterCaps | undefined) =>
  g.kinds.some((k) => optionState(k, caps, "printer").reason === null);

export function Print({ upload = uploadPrint }: { upload?: typeof uploadPrint } = {}) {
  const status = useStatus();
  const connected = useConnected();
  const notFound = useNothingFound();
  const printers = useEnabledPrinters();
  const [printer, choose] = useDeviceChoice("printer", printers);
  const id = printer?.id;
  const available = printer?.available ?? false;
  // Unknown until the printer is connected, so nothing is offered then.
  const caps = available ? printer?.capabilities : undefined;
  // What the printer takes as is, plus what pagewire converts for it.
  const formats = [
    ...(printer?.formats ?? []),
    ...(printer?.convertedFormats ?? []).map((c) => c.from),
  ];
  const config = usePrintConfig(status?.config.etag, status?.config.state);
  const maxUploadMiB = config?.print.maxUploadMiB ?? fallbackMaxUploadMiB;
  const configured = config?.printers.find((p) => p.id === id)?.defaults;

  // Choices are kept per printer, so switching printers recomputes every control from its own.
  const [overrides, setOverrides] = useState<Record<string, PrintOptions>>({});
  const mine = (id && overrides[id]) || {};
  const chosen: PrintOptions = { ...configured, ...mine };
  const set = (patch: Partial<PrintOptions>) => {
    if (id) setOverrides((o) => ({ ...o, [id]: { ...o[id], ...patch } }));
  };
  const copiesMax = copiesLimit(caps);
  const copies = mine.copies ?? 1;
  const copiesValid =
    copiesMax === 0 || (Number.isInteger(copies) && copies >= 1 && copies <= copiesMax);
  const request = toRequest("printer", caps, chosen);
  const sentCopies = request.copies ?? 1;

  const [items, setItems] = useState<Item[]>([]);
  const [sending, setSending] = useState(false);
  const nextKey = useRef(0);
  const update = (key: number, patch: Partial<Item>) =>
    setItems((list) => list.map((i) => (i.key === key ? { ...i, ...patch } : i)));

  function addFiles(files: File[]) {
    const added = files.map((file) => ({
      key: nextKey.current++,
      file,
      format: undefined,
      progress: null,
      error: null,
    }));
    setItems((list) => [...list, ...added]);
    for (const item of added) {
      item.file
        .slice(0, 512)
        .arrayBuffer()
        .then(
          (b) => update(item.key, { format: detectFormat(new Uint8Array(b)) }),
          () => update(item.key, { format: null }),
        );
    }
  }

  const printable = items.filter(
    (i) => i.format !== undefined && problemFor(i, formats, maxUploadMiB) === null,
  );
  const checking = items.some((i) => i.format === undefined);
  // Not `sending`: the button shows pending instead, so keyboard focus stays on it.
  const canPrint = available && connected && copiesValid && !checking && printable.length > 0;

  async function printAll() {
    if (!id) return;
    setSending(true);
    // One job per file, one upload at a time.
    for (const item of printable) {
      update(item.key, { progress: 0, error: null });
      try {
        const { warnings } = await upload(id, item.file, request, (f) =>
          update(item.key, { progress: f }),
        );
        // The job event adds the job to the list below; the file row is done.
        setItems((list) => list.filter((i) => i.key !== item.key));
        toast({
          title: t.sent(item.file.name),
          description: warnings.map((w) => warningText(w, "printer")).join(" ") || undefined,
          tone: "ok",
        });
      } catch (err) {
        update(item.key, { progress: null, error: problemMessage(err) });
        toast({
          title: t.failed(item.file.name),
          description: problemMessage(err),
          tone: "problem",
        });
      }
    }
    setSending(false);
  }

  const accepted = [...new Set(formats.map(baseType).filter((f) => f !== UNKNOWN))].map(formatName);
  const open = printerGroups.filter((g) => !MORE.has(g.id));
  const more = printerGroups.filter((g) => MORE.has(g.id));
  const summary = [
    request.media && optionLabel("media", request.media, "printer"),
    request.sides && optionLabel("sides", request.sides, "printer").toLowerCase(),
  ].filter((x) => x !== undefined);

  return (
    <div className="print-view">
      <PageHeader title={strings.nav.print} description={t.description} />
      <DevicePicker kind="printer" devices={printers} selected={id} onSelect={choose} />

      {!available && (
        <Card className="print-unavailable">
          {status === null ? (
            <p className="print-subtle">{t.unavailable.loading}</p>
          ) : (
            <EmptyState
              icon="print"
              title={t.unavailable.title}
              action={
                <LinkButton variant="secondary" href="/">
                  {t.unavailable.seeStatus}
                </LinkButton>
              }
            >
              <p>{printer?.message || t.unavailable.waiting}</p>
              {notFound && <p>{strings.plugInHint}</p>}
            </EmptyState>
          )}
        </Card>
      )}

      <div className="print-grid">
        <Card title={t.files.title} className="print-files">
          <DropZone
            isDisabled={!available}
            onDrop={async (e) => {
              const files = await Promise.all(
                e.items.flatMap((i) =>
                  // A file the browser cannot hand over is skipped.
                  i.kind === "file" ? [i.getFile().catch(() => null)] : [],
                ),
              );
              addFiles(files.filter((f) => f !== null));
            }}
          >
            <Icon name="upload" size={28} className="print-drop-icon" />
            <Text slot="label" className="print-drop-label print-drop-hint">
              {t.files.drop}
            </Text>
            <span className="print-subtle print-drop-hint">{t.files.or}</span>
            <FileTrigger allowsMultiple onSelect={(list) => addFiles(Array.from(list ?? []))}>
              <Button variant="secondary" isDisabled={!available}>
                {t.files.choose}
              </Button>
            </FileTrigger>
          </DropZone>
          {available && (
            <p className="print-subtle">
              {accepted.length > 0
                ? t.files.accepted(listFormat.format(accepted))
                : t.files.acceptsNothing}{" "}
              {t.files.limit(maxUploadMiB)}
            </p>
          )}

          {items.length > 0 && (
            <ul className="print-list">
              {items.map((item) => {
                const problem = item.error ?? problemFor(item, formats, maxUploadMiB);
                const refused = item.format != null && !formatAccepted(item.format, formats);
                const help = refused ? helpFor(item.file.name, item.format) : null;
                return (
                  <li
                    key={item.key}
                    className="print-row"
                    data-problem={problem ? true : undefined}
                  >
                    <Icon name="file" className="print-file-icon" />
                    <span className="print-row-main">
                      <span className="print-name">{item.file.name}</span>
                      {item.progress !== null ? (
                        <ProgressBar
                          label={t.files.uploading(item.file.name)}
                          value={Math.round(item.progress * 100)}
                        />
                      ) : (
                        <span className={problem ? "print-problem" : "print-subtle"}>
                          {problem ??
                            (item.format === undefined ? t.files.checking : t.files.ready)}{" "}
                          <span className="tabular">({t.files.size(item.file.size)})</span>
                        </span>
                      )}
                      {help && <span className="print-subtle">{help}</span>}
                    </span>
                    <Button
                      variant="quiet"
                      className="print-remove"
                      aria-label={t.files.remove(item.file.name)}
                      // The batch in flight was fixed when Print was pressed.
                      isDisabled={sending}
                      onPress={() => setItems((list) => list.filter((i) => i.key !== item.key))}
                    >
                      <Icon name="close" size={18} />
                    </Button>
                  </li>
                );
              })}
            </ul>
          )}
        </Card>

        <div className="print-side">
          <Card title={t.options.title} className="print-options">
            {copiesMax > 0 && (
              <NumberField
                label={t.options.copies}
                minValue={1}
                maxValue={copiesMax}
                step={1}
                value={copies}
                onChange={(v) => set({ copies: v })}
                errorMessage={copiesValid ? undefined : t.options.copiesError(copiesMax)}
              />
            )}
            {open.map((g) => (
              <Group key={g.id} group={g} caps={caps} chosen={chosen} set={set} />
            ))}
            {more.some((g) => shows(g, caps)) && (
              <Disclosure title={t.options.more}>
                {more.map((g) => (
                  <Group key={g.id} group={g} caps={caps} chosen={chosen} set={set} />
                ))}
              </Disclosure>
            )}
            {status !== null && !caps && <p className="print-subtle">{t.options.unknown}</p>}
            {caps && copiesMax === 0 && !printerGroups.some((g) => shows(g, caps)) && (
              <p className="print-subtle">{t.options.none}</p>
            )}
          </Card>

          <div className="print-submit">
            <p className="print-subtle">{t.summary(sentCopies, summary)}</p>
            <Button
              variant="primary"
              isDisabled={!canPrint && !sending}
              isPending={sending}
              onPress={() => void printAll()}
            >
              <Icon name="print" size={18} />
              {t.submit(printable.length, items.length, sentCopies)}
            </Button>
          </div>
        </div>

        <JobList printerId={id} available={available} />
      </div>
    </div>
  );
}
