// Typed client for /api/v1. Every response body is parsed with the generated Zod schemas.
import { z } from "zod";
import {
  type Config,
  type ConfigResponse,
  ConfigResponseSchema,
  EULASchema,
  type FieldError,
  type Job,
  JobSchema,
  type PrintOptions,
  type Problem,
  ProblemSchema,
  type Scan,
  ScannerCapsSchema,
  ScanSchema,
  type ScanSettings,
  SetupSchema,
  StatusSchema,
} from "../gen/api";
import { strings } from "../strings";

export const API = "/api/v1";

/** An RFC 9457 problem from the server (or a synthesised one for network and parse failures). */
export class ProblemError extends Error {
  readonly type: string;
  readonly status: number;
  readonly title: string;
  readonly detail: string;
  readonly errors: FieldError[];

  constructor(p: Problem) {
    super(p.detail || p.title || p.type);
    this.name = "ProblemError";
    this.type = p.type;
    this.status = p.status;
    this.title = p.title;
    this.detail = p.detail;
    this.errors = p.errors;
  }
}

/** A 412 on PUT or PATCH config. `current` is the file on disk now (valid or invalid). */
export class ConflictError extends Error {
  readonly current: ConfigResponse;

  constructor(current: ConfigResponse) {
    super(strings.problems.byStatus[412]);
    this.name = "ConflictError";
    this.current = current;
  }

  get etag(): string {
    return this.current.etag;
  }
}

/** User-facing text for any error a client call can throw. */
export function problemMessage(err: unknown): string {
  if (err instanceof ConflictError) return err.message;
  if (err instanceof ProblemError) {
    const byType = (strings.problems as Record<string, unknown>)[err.type];
    if (typeof byType === "string") return byType;
    return (
      err.detail || strings.problems.byStatus[err.status] || err.title || strings.problems.unknown
    );
  }
  return strings.problems.unknown;
}

/** The server's ETags are opaque strings; HTTP preconditions want them quoted. */
export function quoteEtag(etag: string): string {
  return etag.startsWith('"') || etag.startsWith("W/") ? etag : `"${etag}"`;
}

function synthetic(type: string, status: number, title: string, detail = ""): ProblemError {
  return new ProblemError({ type, status, title, detail, errors: [] });
}

async function problemFrom(res: Response): Promise<ProblemError> {
  const body = await res.json().catch(() => null);
  const parsed = ProblemSchema.safeParse(body);
  if (parsed.success) return new ProblemError(parsed.data);
  return synthetic("about:blank", res.status, res.statusText || `HTTP ${res.status}`);
}

type Options = {
  method?: string;
  body?: unknown;
  headers?: Record<string, string>;
  contentType?: string;
};

async function send(path: string, opts: Options = {}): Promise<Response> {
  const headers: Record<string, string> = { Accept: "application/json", ...opts.headers };
  let body: string | undefined;
  if (opts.body !== undefined) {
    headers["Content-Type"] = opts.contentType ?? "application/json";
    body = JSON.stringify(opts.body);
  }
  try {
    return await fetch(`${API}${path}`, { method: opts.method ?? "GET", headers, body });
  } catch (err) {
    throw synthetic("network-error", 0, "Network error", err instanceof Error ? err.message : "");
  }
}

function parse<S extends z.ZodType>(schema: S, data: unknown): z.infer<S> {
  const r = schema.safeParse(data);
  if (!r.success) {
    console.error("printscan: unexpected response shape", z.prettifyError(r.error));
    throw synthetic("bad-response", 0, "Unexpected response", z.prettifyError(r.error));
  }
  return r.data;
}

async function json<S extends z.ZodType>(
  schema: S,
  path: string,
  opts?: Options,
): Promise<z.infer<S>> {
  const res = await send(path, opts);
  if (!res.ok) throw await problemFrom(res);
  return parse(schema, await res.json());
}

async function empty(path: string, opts?: Options): Promise<void> {
  const res = await send(path, opts);
  if (!res.ok) throw await problemFrom(res);
}

async function configWrite(opts: Options): Promise<ConfigResponse> {
  const res = await send("/config", opts);
  if (res.status === 412) throw new ConflictError(parse(ConfigResponseSchema, await res.json()));
  if (!res.ok) throw await problemFrom(res);
  return parse(ConfigResponseSchema, await res.json());
}

export const api = {
  getConfig: () => json(ConfigResponseSchema, "/config"),
  rawConfigUrl: `${API}/config/raw`,
  /** Wizard: create the file. A 412 means another session created it first. */
  createConfig: (config: Config) =>
    configWrite({ method: "PUT", body: config, headers: { "If-None-Match": "*" } }),
  replaceConfig: (config: Config, etag: string) =>
    configWrite({ method: "PUT", body: config, headers: { "If-Match": quoteEtag(etag) } }),
  /** RFC 7396 merge patch against the file with `etag`. */
  patchConfig: (patch: Record<string, unknown>, etag: string) =>
    configWrite({
      method: "PATCH",
      body: patch,
      contentType: "application/merge-patch+json",
      headers: { "If-Match": quoteEtag(etag) },
    }),
  getSetup: () => json(SetupSchema, "/setup"),
  /** A vendor licence and its text, read by the device agent from the verified archive. */
  getEula: (id: string) => json(EULASchema, `/eulas/${encodeURIComponent(id)}`),
  getStatus: () => json(StatusSchema, "/status"),
  getJobs: () => json(z.array(JobSchema), "/jobs"),
  getJob: (id: number) => json(JobSchema, `/jobs/${id}`),
  cancelJob: (id: number) => empty(`/jobs/${id}`, { method: "DELETE" }),
  getScanner: () => json(ScannerCapsSchema, "/scanner"),
  startScan: (settings: ScanSettings) =>
    json(ScanSchema, "/scans", { method: "POST", body: settings }),
  getScans: () => json(z.array(ScanSchema), "/scans"),
  getScan: (id: string) => json(ScanSchema, `/scans/${encodeURIComponent(id)}`),
  /**
   * DELETE /scans/{id}: cancels a running scan (202, the scan as it is now; a `scan` event brings
   * the canceled state) or deletes a finished one (204, null).
   */
  deleteScan: async (id: string): Promise<Scan | null> => {
    const res = await send(`/scans/${encodeURIComponent(id)}`, { method: "DELETE" });
    if (!res.ok) throw await problemFrom(res);
    return res.status === 202 ? parse(ScanSchema, await res.json()) : null;
  },
  scanPageUrl: (id: string, n: number) => `${API}/scans/${encodeURIComponent(id)}/pages/${n}`,
  scanDownloadUrl: (id: string) => `${API}/scans/${encodeURIComponent(id)}/download`,
};

/** The print request body: `options` must come before `file` (the server rejects any other order). */
export function printFormData(file: Blob & { name?: string }, options: PrintOptions): FormData {
  const fd = new FormData();
  fd.append("options", JSON.stringify(options));
  fd.append("file", file, file.name ?? "document");
  return fd;
}

/** Uploads one document with progress (fetch cannot report upload progress, XHR can). */
export function uploadPrint(
  file: Blob & { name?: string },
  options: PrintOptions,
  onProgress?: (fraction: number) => void,
  newXhr: () => XMLHttpRequest = () => new XMLHttpRequest(),
): Promise<Job> {
  return new Promise((resolve, reject) => {
    const xhr = newXhr();
    xhr.open("POST", `${API}/print`);
    xhr.setRequestHeader("Accept", "application/json");
    xhr.upload.onprogress = (e) => {
      if (e.lengthComputable && e.total > 0) onProgress?.(e.loaded / e.total);
    };
    xhr.onerror = () => reject(synthetic("network-error", 0, "Network error"));
    xhr.onload = () => {
      let body: unknown = null;
      try {
        body = JSON.parse(xhr.responseText);
      } catch {
        // handled below
      }
      if (xhr.status >= 200 && xhr.status < 300) {
        try {
          resolve(parse(JobSchema, body));
        } catch (err) {
          reject(err);
        }
        return;
      }
      const p = ProblemSchema.safeParse(body);
      reject(
        p.success
          ? new ProblemError(p.data)
          : synthetic("about:blank", xhr.status, xhr.statusText || `HTTP ${xhr.status}`),
      );
    };
    xhr.send(printFormData(file, options));
  });
}
