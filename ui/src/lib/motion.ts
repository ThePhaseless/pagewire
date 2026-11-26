// Gates for motion: mount is not arrival. DESIGN.md (Motion) explains the rules.
import { useCallback, useRef } from "react";
import { api } from "../api/client";
import { useJobs } from "../api/events";

/**
 * True once `value` has differed from its baseline; stays true. The baseline is the value at the
 * first render where `armed` is true (pass false while the value is a placeholder). Once armed, it
 * stays armed.
 */
export function useHasChanged(value: unknown, armed = true): boolean {
  const base = useRef<{ value: unknown } | null>(null);
  const changed = useRef(false);
  if (base.current === null) {
    if (armed) base.current = { value };
  } else if (!Object.is(value, base.current.value)) changed.current = true;
  return changed.current;
}

/** useJobs plus `arrived(id)`: true for a job that was not in the current printer's first successful load. */
export function useJobsWithArrivals(printerId: string | undefined) {
  const base = useRef<{ id: string; ids: ReadonlySet<number> } | null>(null);
  const current = useRef(printerId);
  if (current.current !== printerId) {
    current.current = printerId; // a printer switch starts a fresh list, so a fresh baseline
    base.current = null;
  }
  // Stable on purpose: useJobs refetches whenever its getJobs changes.
  const getJobs = useCallback(async (id: string) => {
    const list = await api.getJobs(id); // read api.getJobs at call time so test spies apply
    if (base.current === null && current.current === id)
      base.current = { id, ids: new Set(list.map((j) => j.id)) };
    return list;
  }, []);
  const result = useJobs(printerId, getJobs);
  const b = base.current;
  const ids = b !== null && b.id === printerId ? b.ids : undefined;
  return { ...result, arrived: (id: number) => ids !== undefined && !ids.has(id) };
}
