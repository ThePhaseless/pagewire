// One EventSource for the whole app: Status, config, job and scan events from /api/v1/events.
import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
  useSyncExternalStore,
} from "react";
import type { z } from "zod";
import {
  type ConfigEvent,
  ConfigEventSchema,
  type Job,
  JobSchema,
  type Scan,
  ScanSchema,
  type Status,
  StatusSchema,
} from "../gen/api";
import { enabled } from "../lib/devices";
import { API, api } from "./client";

const schemas = {
  status: StatusSchema,
  config: ConfigEventSchema,
  job: JobSchema,
  scan: ScanSchema,
} satisfies Record<string, z.ZodType>;

export type EventType = keyof typeof schemas;
export type EventPayload = { status: Status; config: ConfigEvent; job: Job; scan: Scan };

type EventSourceLike = Pick<EventSource, "addEventListener" | "close"> & {
  onopen: ((this: EventSource, ev: Event) => unknown) | null;
  onerror: ((this: EventSource, ev: Event) => unknown) | null;
};

export type StoreOptions = {
  url?: string;
  newEventSource?: (url: string) => EventSourceLike;
  /** Loads the first Status before the stream delivers one. */
  loadStatus?: () => Promise<Status>;
  minBackoffMs?: number;
  maxBackoffMs?: number;
  /** How long pagewire may probe without finding a device before useNothingFound says so. */
  notFoundAfterMs?: number;
};

export type Snapshot = {
  status: Status | null;
  connected: boolean;
  /** Date.now() of the last event received, or null. */
  lastEventAt: number | null;
};

export function createEventStore(opts: StoreOptions = {}) {
  const url = opts.url ?? `${API}/events`;
  const newEventSource = opts.newEventSource ?? ((u: string) => new EventSource(u));
  const minBackoff = opts.minBackoffMs ?? 1000;
  const maxBackoff = opts.maxBackoffMs ?? 30_000;

  let snapshot: Snapshot = { status: null, connected: false, lastEventAt: null };
  const latest: Partial<EventPayload> = {};
  const listeners = new Set<() => void>();
  const typed = new Map<EventType, Set<(payload: never) => void>>();
  const reconnectListeners = new Set<() => void>();
  let source: EventSourceLike | null = null;
  let backoff = minBackoff;
  let timer: ReturnType<typeof setTimeout> | undefined;
  let everOpened = false;
  let running = false;

  const set = (patch: Partial<Snapshot>) => {
    snapshot = { ...snapshot, ...patch };
    for (const l of listeners) l();
  };

  function dispatch<T extends EventType>(type: T, data: string) {
    let payload: EventPayload[T];
    try {
      payload = schemas[type].parse(JSON.parse(data)) as EventPayload[T];
    } catch (err) {
      console.error(`pagewire: dropped malformed "${type}" event`, err);
      return;
    }
    latest[type] = payload;
    set(
      type === "status"
        ? { status: payload as Status, lastEventAt: Date.now() }
        : { lastEventAt: Date.now() },
    );
    for (const fn of typed.get(type) ?? []) (fn as (p: EventPayload[T]) => void)(payload);
  }

  function connect() {
    if (!running) return;
    const es = newEventSource(url);
    source = es;
    es.onopen = () => {
      backoff = minBackoff;
      set({ connected: true });
      if (everOpened) for (const fn of reconnectListeners) fn();
      everOpened = true;
    };
    es.onerror = () => {
      // Close and retry ourselves: the browser gives up for good on an HTTP error response.
      es.close();
      if (source === es) source = null;
      set({ connected: false });
      if (!running) return;
      timer = setTimeout(connect, backoff);
      backoff = Math.min(backoff * 2, maxBackoff);
    };
    for (const type of Object.keys(schemas) as EventType[]) {
      es.addEventListener(type, (e) => dispatch(type, (e as MessageEvent<string>).data));
    }
  }

  return {
    notFoundAfterMs: opts.notFoundAfterMs ?? 5000,
    start() {
      if (running) return;
      running = true;
      connect();
      opts.loadStatus?.().then(
        (s) => {
          if (snapshot.status === null) set({ status: s });
        },
        () => {
          // The stream delivers Status too; the offline banner covers a dead server.
        },
      );
    },
    stop() {
      running = false;
      clearTimeout(timer);
      source?.close();
      source = null;
      set({ connected: false });
    },
    getSnapshot: () => snapshot,
    subscribe(fn: () => void) {
      listeners.add(fn);
      return () => listeners.delete(fn);
    },
    /** Calls `fn` for every event of `type`. Returns the unsubscribe function. */
    on<T extends EventType>(type: T, fn: (payload: EventPayload[T]) => void) {
      const fns = typed.get(type) ?? new Set();
      typed.set(type, fns);
      const f = fn as (payload: never) => void;
      fns.add(f);
      return () => {
        fns.delete(f);
      };
    },
    latest<T extends EventType>(type: T): EventPayload[T] | null {
      return (latest[type] as EventPayload[T] | undefined) ?? null;
    },
    /** Calls `fn` each time the stream reconnects after a drop (not on the first connect). */
    onReconnect(fn: () => void) {
      reconnectListeners.add(fn);
      return () => {
        reconnectListeners.delete(fn);
      };
    },
  };
}

export type EventStore = ReturnType<typeof createEventStore>;

export const defaultStore = createEventStore({ loadStatus: api.getStatus });
export const EventsContext = createContext<EventStore>(defaultStore);

function useSnapshot<T>(select: (s: Snapshot) => T): T {
  const store = useContext(EventsContext);
  return useSyncExternalStore(store.subscribe, () => select(store.getSnapshot()));
}

export const useStatus = () => useSnapshot((s) => s.status);
export const useConnected = () => useSnapshot((s) => s.connected);
export const useLastEventAt = () => useSnapshot((s) => s.lastEventAt);

/** The latest payload of one event type (null until the first one arrives). */
export function useEvent<T extends EventType>(type: T): EventPayload[T] | null {
  const store = useContext(EventsContext);
  const subscribe = useCallback((fn: () => void) => store.on(type, fn), [store, type]);
  return useSyncExternalStore(subscribe, () => store.latest(type));
}

/** The enabled printers in Status order ([] until the first Status). */
export function useEnabledPrinters() {
  const printers = useStatus()?.printers;
  return useMemo(() => enabled(printers ?? []), [printers]);
}

/** The enabled scanners in Status order ([] until the first Status). */
export function useEnabledScanners() {
  const scanners = useStatus()?.scanners;
  return useMemo(() => enabled(scanners ?? []), [scanners]);
}

/**
 * True once the overall phase has been probing for longer than the store's notFoundAfterMs while
 * no detected device can print or scan: pagewire is looking for USB devices (no config yet) and
 * has found nothing it can use. Probing lasts the whole wizard, so found devices end it.
 */
export function useNothingFound(): boolean {
  const store = useContext(EventsContext);
  const status = useStatus();
  const searching =
    status?.phase === "probing" && !status.devices.some((d) => d.canPrint || d.canScan);
  const [late, setLate] = useState(false);
  useEffect(() => {
    setLate(false);
    if (!searching) return;
    const t = setTimeout(() => setLate(true), store.notFoundAfterMs);
    return () => clearTimeout(t);
  }, [searching, store]);
  return searching && late;
}

export const MAX_JOBS = 20;

/** Newest first by creation time (then id), at most MAX_JOBS, one entry per id. */
export function mergeJobs(current: Job[], incoming: Job[]): Job[] {
  const byId = new Map(current.map((j) => [j.id, j]));
  for (const j of incoming) byId.set(j.id, j);
  return [...byId.values()]
    .sort((a, b) => Date.parse(b.createdAt) - Date.parse(a.createdAt) || b.id - a.id)
    .slice(0, MAX_JOBS);
}

/**
 * The recent print jobs of one printer. Loads GET /printers/{id}/jobs, merges that printer's `job`
 * events by id, and refetches on SSE reconnect and whenever the printer's state changes (jobs sent
 * over IPP produce no events). It only asks while the printer is available, since the route answers
 * 503 otherwise. A new printer id starts from an empty list.
 */
export function useJobs(
  printerId: string | undefined,
  getJobs: (printerId: string) => Promise<Job[]> = api.getJobs,
) {
  const store = useContext(EventsContext);
  const printer = useStatus()?.printers.find((p) => p.id === printerId);
  const available = printer?.available ?? false;
  const printerState = printer?.state;
  const [jobs, setJobs] = useState<Job[]>([]);
  const [error, setError] = useState<unknown>(null);
  const availableRef = useRef(available);
  availableRef.current = available;
  const idRef = useRef(printerId);
  idRef.current = printerId;

  const refetch = useCallback(async () => {
    if (printerId === undefined) return;
    try {
      const list = await getJobs(printerId);
      // A reply for the printer the page has since switched away from.
      if (idRef.current !== printerId) return;
      setJobs(mergeJobs([], list));
      setError(null);
    } catch (err) {
      if (idRef.current === printerId) setError(err);
    }
  }, [getJobs, printerId]);

  // biome-ignore lint/correctness/useExhaustiveDependencies: printerId is the reset trigger
  useEffect(() => {
    setJobs([]);
    setError(null);
  }, [printerId]);

  // biome-ignore lint/correctness/useExhaustiveDependencies: printerState is intentionally a refetch trigger
  useEffect(() => {
    if (available) void refetch();
  }, [available, printerState, refetch]);

  useEffect(
    () =>
      store.onReconnect(() => {
        if (availableRef.current) void refetch();
      }),
    [store, refetch],
  );
  useEffect(
    () =>
      store.on("job", (job) => {
        if (job.printer === printerId) setJobs((prev) => mergeJobs(prev, [job]));
      }),
    [store, printerId],
  );

  return { jobs, error, refetch };
}
