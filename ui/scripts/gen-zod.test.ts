import { describe, expect, test } from "bun:test";
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { z } from "zod";
import { expr, generate } from "./gen-zod.ts";

const ui = join(import.meta.dir, "..");
const realSchema = (f: string) => JSON.parse(readFileSync(join(ui, "../schema", f), "utf8"));

// Evaluates a generated expression that references no $defs.
const evalExpr = (node: unknown) => new Function("z", `return ${expr(node)}`)(z) as z.ZodType;

describe("keywords", () => {
  const cases: [string, unknown, string][] = [
    ["string", { type: "string" }, "z.string()"],
    [
      "string limits and pattern",
      { type: "string", minLength: 1, maxLength: 63, pattern: "^[a-z]+$" },
      'z.string().min(1).max(63).regex(new RegExp("^[a-z]+$", "u"))',
    ],
    ["date-time", { type: "string", format: "date-time" }, "z.iso.datetime({ offset: true })"],
    ["integer bounds", { type: "integer", minimum: 1, maximum: 99 }, "z.int().min(1).max(99)"],
    ["number", { type: "number" }, "z.number()"],
    ["boolean", { type: "boolean" }, "z.boolean()"],
    ["enum", { type: "string", enum: ["a", "b"] }, 'z.enum(["a","b"])'],
    ["const", { type: "string", const: "printscan/v1" }, 'z.literal("printscan/v1")'],
    [
      "array bounds",
      { type: "array", items: { type: "string" }, minItems: 1, maxItems: 3 },
      "z.array(z.string()).min(1).max(3)",
    ],
    [
      "uniqueItems",
      { type: "array", items: { type: "integer" }, uniqueItems: true },
      'z.array(z.int()).refine((a) => new Set(a.map((v) => JSON.stringify(v))).size === a.length, { message: "Items must be unique" })',
    ],
    ["$ref", { $ref: "#/$defs/Device", description: "d" }, "DeviceSchema"],
    [
      "nullable oneOf",
      { oneOf: [{ $ref: "#/$defs/Config" }, { type: "null" }] },
      "ConfigSchema.nullable()",
    ],
    ["nullable anyOf", { anyOf: [{ type: "null" }, { type: "string" }] }, "z.string().nullable()"],
    ["nullable type array", { type: ["string", "null"] }, "z.string().nullable()"],
    [
      "strict object: non-required properties are optional, default is not applied",
      {
        type: "object",
        additionalProperties: false,
        required: ["a"],
        properties: {
          a: { type: "string", description: "first" },
          b: { type: "integer" },
          c: { type: "string", default: "x" },
        },
      },
      "z.strictObject({\n  /** first */\n  a: z.string(),\n  b: z.int().optional(),\n  c: z.string().optional(),\n})",
    ],
    ["plain object", { type: "object" }, "z.object({})"],
  ];
  for (const [name, node, want] of cases) {
    test(name, () => expect(expr(node)).toBe(want));
  }

  test("a non-required property becomes optional", () => {
    const s = evalExpr({ type: "object", properties: { a: { type: "string" } } });
    expect(s.parse({})).toEqual({});
  });

  test("both nullable forms accept null", () => {
    expect(evalExpr({ type: ["integer", "null"] }).parse(null)).toBeNull();
    expect(evalExpr({ oneOf: [{ type: "string" }, { type: "null" }] }).parse(null)).toBeNull();
  });

  test("uniqueItems rejects duplicates", () => {
    const s = evalExpr({ type: "array", items: { type: "integer" }, uniqueItems: true });
    expect(s.safeParse([1, 2]).success).toBe(true);
    expect(s.safeParse([1, 1]).success).toBe(false);
  });
});

describe("throws", () => {
  const bad: [string, unknown][] = [
    ["unknown keyword", { type: "string", contentEncoding: "base64" }],
    ["unsupported format", { type: "string", format: "email" }],
    [
      "schema-valued additionalProperties",
      { type: "object", additionalProperties: { type: "string" } },
    ],
    ["additionalProperties true", { type: "object", additionalProperties: true }],
    ["oneOf without null", { oneOf: [{ type: "string" }, { type: "integer" }] }],
    [
      "oneOf with three branches",
      { oneOf: [{ type: "string" }, { type: "null" }, { type: "integer" }] },
    ],
    ["type list without null", { type: ["string", "integer"] }],
    ["non-local $ref", { $ref: "other.json#/x" }],
    ["unknown type", { type: "tuple" }],
    ["allOf", { allOf: [{ type: "string" }] }],
    ["numeric enum", { type: "integer", enum: [1, 2] }],
  ];
  for (const [name, node] of bad) {
    test(name, () => expect(() => expr(node)).toThrow());
  }

  test("unknown top-level keyword", () => {
    expect(() => generate({ $defs: {}, if: {} }, "x.json")).toThrow(/if/);
  });

  test("$ref to a missing definition", () => {
    expect(() => generate({ $defs: { A: { $ref: "#/$defs/B" } } }, "x.json")).toThrow(/B/);
  });
});

describe("generate", () => {
  test("orders definitions by dependency and exports schema and type", () => {
    const out = generate(
      {
        $schema: "https://json-schema.org/draft/2020-12/schema",
        $id: "x",
        $ref: "#/$defs/A",
        $defs: {
          A: { type: "object", properties: { b: { $ref: "#/$defs/B" } } },
          B: { type: "boolean" },
        },
      },
      "x.json",
    );
    expect(out.indexOf("export const BSchema")).toBeLessThan(out.indexOf("export const ASchema"));
    expect(out).toContain("export type A = z.infer<typeof ASchema>;");
    // Before any schema is built, so zod never probes eval under the CSP.
    expect(out.indexOf("z.config({ jitless: true });")).toBeLessThan(out.indexOf("export const"));
  });

  test("two runs are byte-identical", () => {
    for (const f of ["config.schema.json", "api.schema.json"]) {
      expect(generate(realSchema(f), f)).toBe(generate(realSchema(f), f));
    }
  });

  test("committed src/gen matches the schema", () => {
    expect(readFileSync(join(ui, "src/gen/config.ts"), "utf8")).toBe(
      generate(realSchema("config.schema.json"), "config.schema.json"),
    );
    expect(readFileSync(join(ui, "src/gen/api.ts"), "utf8")).toBe(
      generate(realSchema("api.schema.json"), "api.schema.json"),
    );
  });

  test("date-time accepts Z, offsets and fractional seconds", async () => {
    const dt = evalExpr({ type: "string", format: "date-time" });
    for (const ok of [
      "2026-11-24T17:00:00Z",
      "2026-11-24T17:00:00.123456789+01:00",
      "0001-01-01T00:00:00Z",
    ]) {
      expect(dt.safeParse(ok).success).toBe(true);
    }
    expect(dt.safeParse("2026-11-24 17:00").success).toBe(false);

    const { JobSchema } = await import("../src/gen/api.ts");
    const job = { id: 1, name: "a", state: "pending", reasons: [], createdAt: "" };
    expect(
      JobSchema.safeParse({ ...job, createdAt: "2026-11-24T17:00:00.123456789+01:00" }).success,
    ).toBe(true);
    expect(JobSchema.safeParse({ ...job, createdAt: "2026-11-24 17:00" }).success).toBe(false);
  });

  test("generated output for the real schemas type-checks", () => {
    const base = join(ui, "node_modules/.cache");
    mkdirSync(base, { recursive: true });
    const dir = mkdtempSync(join(base, "gen-zod-"));
    try {
      writeFileSync(join(dir, "config.ts"), generate(realSchema("config.schema.json"), "c"));
      writeFileSync(join(dir, "api.ts"), generate(realSchema("api.schema.json"), "a"));
      writeFileSync(
        join(dir, "tsconfig.json"),
        JSON.stringify({
          compilerOptions: {
            strict: true,
            noUncheckedIndexedAccess: true,
            module: "Preserve",
            moduleResolution: "bundler",
            target: "ES2023",
            noEmit: true,
            skipLibCheck: true,
            types: [],
          },
          files: ["config.ts", "api.ts"],
        }),
      );
      const r = Bun.spawnSync([join(ui, "node_modules/.bin/tsc"), "-p", dir]);
      expect(r.stdout.toString() + r.stderr.toString()).toBe("");
      expect(r.exitCode).toBe(0);
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  }, 60_000);
});
