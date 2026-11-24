// Turns the JSON Schema subset that invopop/jsonschema emits for printscan into Zod 4 source.
// It throws on any keyword it does not handle, so a schema change can never be silently ignored.
import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { basename, join } from "node:path";

type Node = Record<string, unknown>;

const ANNOTATIONS = ["description", "title", "default"];
const STRING_KEYS = ["pattern", "minLength", "maxLength", "format", "enum", "const"];
const NUMBER_KEYS = ["minimum", "maximum", "enum", "const"];
const ARRAY_KEYS = ["items", "minItems", "maxItems", "uniqueItems"];
const OBJECT_KEYS = ["properties", "required", "additionalProperties"];

class SchemaError extends Error {}

function fail(path: string, msg: string): never {
  throw new SchemaError(`${path || "/"}: ${msg}`);
}

function isNode(v: unknown): v is Node {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

function checkKeys(node: Node, allowed: string[], path: string) {
  for (const k of Object.keys(node)) {
    if (!allowed.includes(k) && !ANNOTATIONS.includes(k)) fail(path, `unsupported keyword "${k}"`);
  }
}

function lit(v: unknown): string {
  return JSON.stringify(v);
}

function refName(ref: unknown, path: string): string {
  if (typeof ref !== "string" || !ref.startsWith("#/$defs/"))
    fail(path, `unsupported $ref ${lit(ref)}`);
  return ref.slice("#/$defs/".length);
}

function indent(s: string, pad: string): string {
  return s.replaceAll("\n", `\n${pad}`);
}

function comment(text: unknown, pad = ""): string {
  if (typeof text !== "string" || text === "") return "";
  return `${pad}/** ${text.replaceAll("*/", "*\\/").replaceAll("\n", " ")} */\n`;
}

/** Returns the Zod expression for one schema node. `refs` collects every $defs name it uses. */
export function expr(node: unknown, path = "", refs: Set<string> = new Set()): string {
  if (!isNode(node)) fail(path, "schema must be an object");

  // Nullable, form 1: oneOf/anyOf with exactly two branches, one of them {type: "null"}.
  for (const key of ["oneOf", "anyOf"] as const) {
    if (!(key in node)) continue;
    checkKeys(node, [key], path);
    const branches = node[key];
    const isNull = (b: unknown) => isNode(b) && b.type === "null" && Object.keys(b).length === 1;
    if (!Array.isArray(branches) || branches.length !== 2 || branches.filter(isNull).length !== 1) {
      fail(path, `${key} is supported only as [schema, {type: "null"}]`);
    }
    const other = branches.find((b) => !isNull(b));
    return `${expr(other, `${path}/${key}`, refs)}.nullable()`;
  }

  if ("$ref" in node) {
    checkKeys(node, ["$ref"], path);
    const name = refName(node.$ref, path);
    refs.add(name);
    return `${name}Schema`;
  }

  // Nullable, form 2: type: [x, "null"].
  if (Array.isArray(node.type)) {
    const types = node.type;
    if (types.length !== 2 || !types.includes("null")) {
      fail(path, `type ${lit(types)} is supported only as [type, "null"]`);
    }
    const base = types.find((t) => t !== "null");
    return `${expr({ ...node, type: base }, path, refs)}.nullable()`;
  }

  if ("const" in node) {
    checkKeys(node, ["type", "const"], path);
    return `z.literal(${lit(node.const)})`;
  }

  if ("enum" in node) {
    checkKeys(node, ["type", "enum"], path);
    const values = node.enum;
    if (
      !Array.isArray(values) ||
      values.length === 0 ||
      !values.every((v) => typeof v === "string")
    ) {
      fail(path, "enum is supported only with string values");
    }
    if (node.type !== undefined && node.type !== "string") fail(path, "enum needs type string");
    return `z.enum(${lit(values)})`;
  }

  switch (node.type) {
    case "string": {
      checkKeys(node, ["type", ...STRING_KEYS], path);
      let out: string;
      if ("format" in node) {
        if (node.format !== "date-time") fail(path, `unsupported format ${lit(node.format)}`);
        out = "z.iso.datetime({ offset: true })";
      } else {
        out = "z.string()";
      }
      if (node.minLength !== undefined) out += `.min(${num(node.minLength, path)})`;
      if (node.maxLength !== undefined) out += `.max(${num(node.maxLength, path)})`;
      if (node.pattern !== undefined) {
        if (typeof node.pattern !== "string") fail(path, "pattern must be a string");
        out += `.regex(new RegExp(${lit(node.pattern)}, "u"))`;
      }
      return out;
    }
    case "integer":
    case "number": {
      checkKeys(node, ["type", ...NUMBER_KEYS], path);
      let out = node.type === "integer" ? "z.int()" : "z.number()";
      if (node.minimum !== undefined) out += `.min(${num(node.minimum, path)})`;
      if (node.maximum !== undefined) out += `.max(${num(node.maximum, path)})`;
      return out;
    }
    case "boolean":
      checkKeys(node, ["type"], path);
      return "z.boolean()";
    case "array": {
      checkKeys(node, ["type", ...ARRAY_KEYS], path);
      if (!("items" in node)) fail(path, "array without items");
      let out = `z.array(${expr(node.items, `${path}/items`, refs)})`;
      if (node.minItems !== undefined) out += `.min(${num(node.minItems, path)})`;
      if (node.maxItems !== undefined) out += `.max(${num(node.maxItems, path)})`;
      if (node.uniqueItems === true) {
        out +=
          '.refine((a) => new Set(a.map((v) => JSON.stringify(v))).size === a.length, { message: "Items must be unique" })';
      } else if (node.uniqueItems !== undefined && node.uniqueItems !== false) {
        fail(path, "uniqueItems must be a boolean");
      }
      return out;
    }
    case "object":
      checkKeys(node, ["type", ...OBJECT_KEYS], path);
      return objectExpr(node, path, refs);
    default:
      fail(path, `unsupported type ${lit(node.type)}`);
  }
}

function num(v: unknown, path: string): number {
  if (typeof v !== "number") fail(path, `expected a number, got ${lit(v)}`);
  return v;
}

function objectExpr(node: Node, path: string, refs: Set<string>): string {
  const ap = node.additionalProperties;
  if (ap !== undefined && ap !== false) fail(path, "only additionalProperties: false is supported");
  const props = node.properties ?? {};
  if (!isNode(props)) fail(path, "properties must be an object");
  const required = node.required ?? [];
  if (!Array.isArray(required)) fail(path, "required must be an array");
  for (const r of required) {
    if (!(r in props)) fail(path, `required property ${lit(r)} is not defined`);
  }
  const lines = Object.entries(props).map(([name, schema]) => {
    const p = `${path}/properties/${name}`;
    let e = expr(schema, p, refs);
    // `default` is documentation here: the server fills defaults, the UI never invents them.
    if (!required.includes(name)) e += ".optional()";
    const key = /^[A-Za-z_$][\w$]*$/.test(name) ? name : lit(name);
    return `${comment(isNode(schema) ? schema.description : undefined, "  ")}  ${key}: ${indent(e, "  ")},`;
  });
  const fn = ap === false ? "z.strictObject" : "z.object";
  return lines.length === 0 ? `${fn}({})` : `${fn}({\n${lines.join("\n")}\n})`;
}

/** Generates a TypeScript module with one schema and one inferred type per $defs entry. */
export function generate(schema: unknown, source: string): string {
  if (!isNode(schema)) fail("", "schema must be an object");
  for (const k of Object.keys(schema)) {
    if (!["$schema", "$id", "$ref", "$defs", "title", "description"].includes(k)) {
      fail("", `unsupported top-level keyword "${k}"`);
    }
  }
  const defs = schema.$defs ?? {};
  if (!isNode(defs)) fail("/$defs", "$defs must be an object");
  if ("$ref" in schema && !(refName(schema.$ref, "") in defs)) {
    fail("", `top-level $ref ${lit(schema.$ref)} has no $defs entry`);
  }

  const bodies = new Map<string, { code: string; refs: Set<string> }>();
  for (const name of Object.keys(defs).sort()) {
    const refs = new Set<string>();
    const code = expr(defs[name], `/$defs/${name}`, refs);
    for (const r of refs)
      if (!(r in defs)) fail(`/$defs/${name}`, `$ref to unknown $defs entry ${lit(r)}`);
    bodies.set(name, { code, refs });
  }

  // Emit in dependency order (alphabetical within that), so every const is defined before use.
  const out: string[] = [];
  const done = new Set<string>();
  const visiting = new Set<string>();
  const visit = (name: string) => {
    if (done.has(name)) return;
    if (visiting.has(name)) fail(`/$defs/${name}`, "recursive $ref is not supported");
    visiting.add(name);
    const body = bodies.get(name);
    if (!body) fail(`/$defs/${name}`, "missing definition");
    for (const r of [...body.refs].sort()) visit(r);
    visiting.delete(name);
    done.add(name);
    const def = defs[name];
    out.push(
      `${comment(isNode(def) ? def.description : undefined)}export const ${name}Schema = ${body.code};\n` +
        `export type ${name} = z.infer<typeof ${name}Schema>;\n`,
    );
  };
  for (const name of bodies.keys()) visit(name);

  return [
    `// Code generated by scripts/gen-zod.ts from schema/${source}. DO NOT EDIT.`,
    'import { z } from "zod";',
    "",
    "// The UI's CSP has no 'unsafe-eval'. jitless must be set before the first z.object() is built,",
    "// or zod probes new Function() and the browser logs a CSP violation.",
    "z.config({ jitless: true });",
    "",
    out.join("\n"),
  ].join("\n");
}

if (import.meta.main) {
  const ui = join(import.meta.dir, "..");
  mkdirSync(join(ui, "src/gen"), { recursive: true });
  for (const [file, out] of [
    ["config.schema.json", "config.ts"],
    ["api.schema.json", "api.ts"],
  ] as const) {
    const src = join(ui, "../schema", file);
    const code = generate(JSON.parse(readFileSync(src, "utf8")), basename(src));
    writeFileSync(join(ui, "src/gen", out), code);
    console.log(`wrote src/gen/${out}`);
  }
}
