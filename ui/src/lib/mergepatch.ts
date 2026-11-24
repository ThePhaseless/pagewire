// RFC 7396 JSON merge patch: the diff between two JSON documents, plus pointer helpers.

type Obj = Record<string, unknown>;

const isObj = (v: unknown): v is Obj => typeof v === "object" && v !== null && !Array.isArray(v);

const same = (a: unknown, b: unknown) => JSON.stringify(a) === JSON.stringify(b);

/**
 * The merge patch that turns `base` into `edited`. Keys missing (or undefined) in `edited` become
 * `null`, so removing an optional object such as `device.usb` deletes it. Arrays are replaced whole.
 * Equal documents give `{}`.
 */
export function diff(base: Obj, edited: Obj): Obj {
  const patch: Obj = {};
  for (const k of Object.keys(base)) {
    if (base[k] !== undefined && edited[k] === undefined) patch[k] = null;
  }
  for (const [k, v] of Object.entries(edited)) {
    if (v === undefined) continue;
    const b = base[k];
    if (isObj(b) && isObj(v)) {
      const sub = diff(b, v);
      if (Object.keys(sub).length > 0) patch[k] = sub;
    } else if (!same(b, v)) {
      patch[k] = v;
    }
  }
  return patch;
}

/** JSON pointers of the patch's leaves (scalars, arrays and nulls), in key order. */
export function changedPaths(patch: Obj, prefix = ""): string[] {
  return Object.entries(patch).flatMap(([k, v]) =>
    isObj(v) ? changedPaths(v, `${prefix}/${k}`) : [`${prefix}/${k}`],
  );
}

/** The value at a JSON pointer (keys without `~` escapes, which config keys never need). */
export function valueAt(doc: unknown, pointer: string): unknown {
  let cur = doc;
  for (const k of pointer.split("/").slice(1)) {
    if (!isObj(cur)) return undefined;
    cur = cur[k];
  }
  return cur;
}
