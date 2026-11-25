import { afterAll, describe, expect, test } from "bun:test";
import { existsSync, mkdtempSync, readdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { build } from "./build.ts";

const dirs: string[] = [];
const tmp = () => {
  const d = mkdtempSync(join(tmpdir(), "pagewire-build-"));
  dirs.push(d);
  return d;
};
afterAll(() => {
  for (const d of dirs) rmSync(d, { recursive: true, force: true });
});

describe("build", () => {
  test("empties outdir except .keep and writes the app with no inline script or style", async () => {
    const out = tmp();
    writeFileSync(join(out, ".keep"), "");
    writeFileSync(join(out, "old.js"), "stale");

    await build(out);

    expect(existsSync(join(out, ".keep"))).toBe(true);
    expect(existsSync(join(out, "old.js"))).toBe(false);
    expect(existsSync(join(out, "index.html"))).toBe(true);
    expect(readFileSync(join(out, "THIRD_PARTY_LICENSES.txt"), "utf8")).toContain("react@19");

    const html = readFileSync(join(out, "index.html"), "utf8");
    // Every <script> must load a file (src=) and have an empty body; no <style> at all.
    for (const m of html.matchAll(/<script\b([^>]*)>([\s\S]*?)<\/script>/gi)) {
      expect(m[1]).toMatch(/\bsrc=/);
      expect(m[2]?.trim()).toBe("");
    }
    expect(html).not.toMatch(/<style\b/i);
    // Stops react-aria from injecting its own <style> at runtime.
    expect(html).toContain('id="react-aria-pressable-style"');
    expect(html).toMatch(/<script\b[^>]*\bsrc="\/assets\//i);
    expect(html).toMatch(/<link\b[^>]*rel="stylesheet"/i);
    // Fonts are same-origin files, never data: URIs (CSP: default-src 'self').
    const assets = readdirSync(join(out, "assets"));
    const css = assets
      .filter((f) => f.endsWith(".css"))
      .map((f) => readFileSync(join(out, "assets", f), "utf8"));
    expect(css.join("")).not.toContain("data:font");
    expect(readdirSync(join(out, "assets/files")).some((f) => f.endsWith(".woff2"))).toBe(true);
  }, 60_000);

  test("creates .keep in an empty outdir", async () => {
    const out = tmp();
    await build(out);
    expect(existsSync(join(out, ".keep"))).toBe(true);
    expect(readFileSync(join(out, ".keep"), "utf8")).toBe("");
  }, 60_000);
});
