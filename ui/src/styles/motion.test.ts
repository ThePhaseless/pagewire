// Guards DESIGN.md's motion rules over every stylesheet: what may animate, and that every duration
// and easing is a token.
import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";

const root = join(import.meta.dir, "..");
const sheets = [...new Bun.Glob("**/*.css").scanSync({ cwd: root })].sort().map((file) => ({
  file,
  css: readFileSync(join(root, file), "utf8").replace(/\/\*[\s\S]*?\*\//g, ""),
}));

/** Splits on `sep` outside parentheses; empty parts dropped. */
function splitTop(value: string, sep: RegExp): string[] {
  const parts: string[] = [];
  let depth = 0;
  let cur = "";
  for (const ch of value) {
    if (ch === "(") depth++;
    if (ch === ")") depth--;
    if (depth === 0 && sep.test(ch)) {
      parts.push(cur);
      cur = "";
    } else cur += ch;
  }
  parts.push(cur);
  return parts.map((p) => p.trim()).filter(Boolean);
}

/** The bodies of every block whose prelude matches `head` (for example `@keyframes x {`). */
function blocks(css: string, head: RegExp): string[] {
  const out: string[] = [];
  for (const m of css.matchAll(new RegExp(head.source, "g"))) {
    let depth = 1;
    let i = (m.index ?? 0) + m[0].length;
    const start = i;
    for (; i < css.length && depth > 0; i++) {
      if (css[i] === "{") depth++;
      if (css[i] === "}") depth--;
    }
    out.push(css.slice(start, i - 1));
  }
  return out;
}

/** `[property, value]` for every declaration; custom-property definitions are exempt. */
const declarations = (css: string) =>
  [...css.matchAll(/([-\w]+)\s*:\s*([^;{}]+)(?=[;}])/g)]
    .map((m) => [m[1] as string, (m[2] as string).trim()] as const)
    .filter(([p]) => !p.startsWith("--"));

const transitionable = new Set([
  "opacity",
  "transform",
  "translate",
  "scale",
  "rotate",
  "clip-path",
  "color",
  "background-color",
  "border-color",
]);
const keyframeable = new Set(["opacity", "transform", "translate", "scale", "rotate", "clip-path"]);
const timed = new Set([
  "transition",
  "animation",
  "transition-duration",
  "animation-duration",
  "animation-delay",
  "transition-delay",
  "transition-timing-function",
  "animation-timing-function",
]);
const easingKeyword =
  /^(linear|ease|ease-in|ease-out|ease-in-out|step-start|step-end)$|^(steps|cubic-bezier|linear)\(/;
const time = /^[-+]?[\d.]+m?s$/;

test("transitions name only opacity, transforms, clip-path and colours", () => {
  const bad: string[] = [];
  for (const { file, css } of sheets)
    for (const [prop, value] of declarations(css)) {
      if (prop !== "transition" && prop !== "transition-property") continue;
      for (const item of splitTop(value, /,/)) {
        const name = splitTop(item, /\s/).find(
          (t) => !t.startsWith("var(") && !time.test(t) && !easingKeyword.test(t),
        );
        // No name means `all`.
        if (name !== "none" && (name === undefined || !transitionable.has(name)))
          bad.push(`${file}: ${prop}: ${value}`);
      }
    }
  expect(bad).toEqual([]);
});

test("every duration and easing is a token", () => {
  const bad: string[] = [];
  for (const { file, css } of sheets)
    for (const [prop, value] of declarations(css)) {
      if (!timed.has(prop)) continue;
      for (const item of splitTop(value, /,/))
        for (const t of splitTop(item, /\s/)) {
          const literal = time.test(t) || easingKeyword.test(t);
          const otherVar = t.startsWith("var(") && !/^var\(--(dur|ease)/.test(t);
          if (literal || otherVar) bad.push(`${file}: ${prop}: ${value} (${t})`);
        }
    }
  expect(bad).toEqual([]);
});

test("keyframes animate only opacity, transforms and clip-path", () => {
  const bad: string[] = [];
  for (const { file, css } of sheets)
    for (const body of blocks(css, /@keyframes\s+[-\w]+\s*\{/))
      for (const [prop, value] of declarations(body))
        if (!keyframeable.has(prop)) bad.push(`${file}: ${prop}: ${value}`);
  expect(bad).toEqual([]);
});

test("reduced motion zeroes the rise but keeps arrival and reveal fades", () => {
  const tokens = sheets.find((s) => s.file === join("styles", "tokens.css"));
  const reduced = blocks(tokens?.css ?? "", /@media\s*\(prefers-reduced-motion:\s*reduce\)\s*\{/);
  expect(reduced).toHaveLength(1);
  const body = reduced[0] ?? "";
  expect(body).toMatch(/--rise:\s*0px/);
  expect(body).not.toMatch(/--dur-(arrive|reveal)\s*:/);
});
