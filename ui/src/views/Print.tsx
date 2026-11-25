// The Print page: pick or drop documents, check them against the printer's formats, choose
// options, upload with progress, and follow and cancel jobs.
import { useEffect, useRef, useState } from "react";
import { Text } from "react-aria-components";
import { api, problemMessage, uploadPrint } from "../api/client";
import { useConnected, useJobs, usePrinterNotFound, useStatus } from "../api/events";
import {
  Button,
  Card,
  DropZone,
  EmptyState,
  FileTrigger,
  Icon,
  InlineError,
  LinkButton,
  NumberField,
  PageHeader,
  ProgressBar,
  Select,
  StatusBadge,
  type Tone,
  toast,
} from "../components/ui";
import type { Config, Job, PrintOptions } from "../gen/api";
import { strings } from "../strings";
import { printStrings as t } from "./Print.strings";
import "./Print.css";

// Mirrors config.Default: what the server applies when the file leaves print defaults out.
const fallback: { options: PrintOptions; maxUploadMiB: number } = {
  options: {
    copies: 1,
    sides: "one-sided",
    colorMode: "auto",
    media: "iso_a4_210x297mm",
    quality: "normal",
  },
  maxUploadMiB: 50,
};

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

const PWG_MEDIA = /^([a-z0-9-]+)_([a-z0-9.-]+)_([0-9.]+)x([0-9.]+)(mm|in)$/;

/** "iso_a4_210x297mm" → "A4"; anything unparsable is shown as is. */
function mediaName(pwg: string): string {
  const [, cls = "", name = ""] = PWG_MEDIA.exec(pwg) ?? [];
  if (!name) return pwg;
  return cls === "iso" || cls === "jis"
    ? name.toUpperCase()
    : name.charAt(0).toUpperCase() + name.slice(1);
}

/** "iso_a4_210x297mm" → "A4 (210 × 297 mm)"; anything unparsable is shown as is. */
export function mediaLabel(pwg: string): string {
  const [, , , w = "", h = "", unit = ""] = PWG_MEDIA.exec(pwg) ?? [];
  return unit ? t.options.mediaSize(mediaName(pwg), w, h, unit) : pwg;
}

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
  processing: "busy",
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

function JobList({ available }: { available: boolean }) {
  const { jobs, error } = useJobs();
  const [canceling, setCanceling] = useState<Set<number>>(new Set());

  async function cancel(job: Job) {
    setCanceling((s) => new Set(s).add(job.id));
    try {
      await api.cancelJob(job.id);
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
            <li key={j.id} className="print-row">
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

export function Print({ upload = uploadPrint }: { upload?: typeof uploadPrint } = {}) {
  const status = useStatus();
  const connected = useConnected();
  const notFound = usePrinterNotFound();
  const printer = status?.printer;
  const available = printer?.available ?? false;
  // What the printer takes as is, plus what pagewire converts for it.
  const formats = [
    ...(printer?.formats ?? []),
    ...(printer?.convertedFormats ?? []).map((c) => c.from),
  ];
  const config = usePrintConfig(status?.config.etag, status?.config.state);
  const maxUploadMiB = config?.print.maxUploadMiB ?? fallback.maxUploadMiB;

  const [overrides, setOverrides] = useState<Partial<PrintOptions>>({});
  const options: PrintOptions = {
    ...fallback.options,
    ...config?.print.defaults,
    ...overrides,
  };
  const set = (patch: Partial<PrintOptions>) => setOverrides((o) => ({ ...o, ...patch }));
  const copiesValid =
    Number.isInteger(options.copies) && options.copies >= 1 && options.copies <= 99;

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
    setSending(true);
    const opts = options;
    // One job per file, one upload at a time.
    for (const item of printable) {
      update(item.key, { progress: 0, error: null });
      try {
        await upload(item.file, opts, (f) => update(item.key, { progress: f }));
        // The job event adds the job to the list below; the file row is done.
        setItems((list) => list.filter((i) => i.key !== item.key));
        toast({ title: t.sent(item.file.name), tone: "ok" });
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

  const mediaIds = [...new Set([...(printer?.media ?? []), options.media])];
  const accepted = [...new Set(formats.map(baseType).filter((f) => f !== UNKNOWN))].map(formatName);

  return (
    <div className="print-view">
      <PageHeader title={strings.nav.print} description={t.description} />

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
              <p>{status.device.message || t.unavailable.waiting}</p>
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

        <Card title={t.options.title} className="print-options">
          <NumberField
            label={t.options.copies}
            minValue={1}
            maxValue={99}
            step={1}
            value={options.copies}
            onChange={(v) => set({ copies: v })}
            errorMessage={copiesValid ? undefined : t.options.copiesError}
          />
          <Select
            label={t.options.media}
            selectedKey={options.media}
            onSelectionChange={(k) => set({ media: String(k) })}
            options={mediaIds.map((id) => ({ id, label: mediaLabel(id) }))}
          />
          <Select
            label={t.options.sides.label}
            selectedKey={options.sides}
            onSelectionChange={(k) => set({ sides: k as PrintOptions["sides"] })}
            options={(["one-sided", "two-sided-long-edge", "two-sided-short-edge"] as const).map(
              (id) => ({ id, label: t.options.sides[id] }),
            )}
          />
          <Select
            label={t.options.colorMode.label}
            selectedKey={options.colorMode}
            onSelectionChange={(k) => set({ colorMode: k as PrintOptions["colorMode"] })}
            options={(["auto", "color", "monochrome"] as const).map((id) => ({
              id,
              label: t.options.colorMode[id],
            }))}
          />
          <Select
            label={t.options.quality.label}
            selectedKey={options.quality}
            onSelectionChange={(k) => set({ quality: k as PrintOptions["quality"] })}
            options={(["draft", "normal", "high"] as const).map((id) => ({
              id,
              label: t.options.quality[id],
            }))}
          />
        </Card>
      </div>

      <div className="print-submit">
        <p className="print-subtle">
          {t.summary(
            copiesValid ? options.copies : 1,
            mediaName(options.media),
            t.options.sides[options.sides].toLowerCase(),
          )}
        </p>
        <Button
          variant="primary"
          isDisabled={!canPrint && !sending}
          isPending={sending}
          onPress={() => void printAll()}
        >
          <Icon name="print" size={18} />
          {t.submit(printable.length, items.length, copiesValid ? options.copies : 1)}
        </Button>
      </div>

      <JobList available={available} />
    </div>
  );
}
