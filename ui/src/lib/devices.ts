// Several printers and scanners: the enabled ones, and the one the user picked last.
import { useCallback, useState } from "react";
import { replaceQueryParam, useQueryParam } from "../router";

/** A printer or scanner. Also the query parameter (`?printer=<id>`) that selects one. */
export type DeviceKind = "printer" | "scanner";

/** The entries that are switched on, in list order. */
export function enabled<T extends { enabled: boolean }>(list: T[]): T[] {
  return list.filter((d) => d.enabled);
}

const storageKey = (kind: DeviceKind) => `pagewire:${kind}`;

// Storage can be unavailable (private mode, blocked cookies); the choice is then not kept.
function stored(kind: DeviceKind): string | null {
  try {
    return localStorage.getItem(storageKey(kind));
  } catch {
    return null;
  }
}

/** Keeps `id` as the device of this kind to preselect next time. */
export function remember(kind: DeviceKind, id: string) {
  try {
    localStorage.setItem(storageKey(kind), id);
  } catch {
    // See stored().
  }
}

/**
 * The device to show: the one `query` names (which is then remembered), else the remembered one,
 * else the first. An id that is not in `list` is ignored and never overwrites the remembered one,
 * so an empty list (before the first Status) changes nothing.
 */
export function pickRemembered<T extends { id: string }>(
  list: T[],
  kind: DeviceKind,
  query: string | null,
): T | undefined {
  const asked = query === null ? undefined : list.find((d) => d.id === query);
  if (asked) {
    remember(kind, asked.id);
    return asked;
  }
  const last = stored(kind);
  return list.find((d) => d.id === last) ?? list[0];
}

/**
 * The selected device of a page with a device picker, from `?<kind>=<id>`, the remembered choice
 * or the first, and the function the picker calls. A choice is remembered and drops the query
 * parameter, so the address no longer names another device.
 */
export function useDeviceChoice<T extends { id: string }>(
  kind: DeviceKind,
  list: T[],
): [T | undefined, (id: string) => void] {
  const query = useQueryParam(kind);
  const [picked, setPicked] = useState<string | null>(null);
  // A query that arrives later (a link to this page) wins over an earlier pick.
  const device = pickRemembered(list, kind, query ?? picked);
  const choose = useCallback(
    (id: string) => {
      remember(kind, id);
      setPicked(id);
      replaceQueryParam(kind, null);
    },
    [kind],
  );
  return [device, choose];
}
