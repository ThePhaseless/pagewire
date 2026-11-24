// Writes THIRD_PARTY_LICENSES.txt for every production npm dependency that ends up in the bundle.
import { existsSync, readdirSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";

type LockEntry = [
  string,
  string,
  {
    dependencies?: Record<string, string>;
    optionalDependencies?: Record<string, string>;
    peerDependencies?: Record<string, string>;
  }?,
  string?,
];

const LICENSE_FILE = /^(licen[cs]e|copying|ofl)([.-].*)?$/i;

// Packages that ship without a licence file, mapped to an installed package by the same
// copyright holder under the same licence. client-only is React's MIT marker package.
const BORROWED_TEXT: Record<string, string> = { "client-only": "react" };

export async function writeLicenses(
  outdir: string,
  ui = join(import.meta.dir, ".."),
): Promise<string> {
  const lock = (await import(join(ui, "bun.lock"))).default as {
    workspaces: Record<string, { dependencies?: Record<string, string> }>;
    packages: Record<string, LockEntry>;
  };

  // key: lockfile package key ("a" or "parent/a" for a nested copy); dir: its node_modules path.
  const seen = new Map<string, { dir: string; optional: boolean }>();
  const queue: { key: string; dir: string; optional: boolean }[] = Object.keys(
    lock.workspaces[""]?.dependencies ?? {},
  ).map((name) => ({ key: name, dir: join(ui, "node_modules", name), optional: false }));
  while (queue.length > 0) {
    const item = queue.shift();
    if (!item || seen.has(item.key)) continue;
    const entry = lock.packages[item.key];
    if (!entry) throw new Error(`bun.lock has no package ${item.key}`);
    seen.set(item.key, { dir: item.dir, optional: item.optional });
    const meta = entry[2] ?? {};
    const peers = Object.keys(meta.peerDependencies ?? {});
    const optionals = Object.keys(meta.optionalDependencies ?? {});
    for (const dep of [...Object.keys(meta.dependencies ?? {}), ...optionals, ...peers]) {
      const optional = item.optional || optionals.includes(dep);
      const nested = `${item.key}/${dep}`;
      if (lock.packages[nested]) {
        queue.push({ key: nested, dir: join(item.dir, "node_modules", dep), optional });
      } else if (lock.packages[dep]) {
        queue.push({ key: dep, dir: join(ui, "node_modules", dep), optional });
      } else if (!peers.includes(dep)) {
        throw new Error(`bun.lock has no package ${dep} (needed by ${item.key})`);
      }
    }
  }

  const sections: string[] = [];
  const missing: string[] = [];
  for (const [key, { dir, optional }] of seen) {
    if (!existsSync(dir)) {
      if (optional) continue; // an optional dependency for another platform
      throw new Error(`${key} is in bun.lock but not installed at ${dir}; run bun install`);
    }
    const pkg = JSON.parse(readFileSync(join(dir, "package.json"), "utf8")) as {
      name: string;
      version: string;
      license?: string;
    };
    const borrowed = BORROWED_TEXT[pkg.name];
    const textDir = borrowed ? join(ui, "node_modules", borrowed) : dir;
    const files = readdirSync(textDir)
      .filter((f) => LICENSE_FILE.test(f))
      .sort();
    if (files.length === 0) {
      missing.push(`${pkg.name}@${pkg.version} (${key})`);
      continue;
    }
    const texts = files.map((f) => readFileSync(join(textDir, f), "utf8").trim()).join("\n\n");
    sections.push(`${pkg.name}@${pkg.version}\nLicense: ${pkg.license ?? "see text"}\n\n${texts}`);
  }
  if (missing.length > 0) throw new Error(`no licence text found for: ${missing.join(", ")}`);

  sections.sort();
  const out = join(outdir, "THIRD_PARTY_LICENSES.txt");
  const rule = `\n\n${"-".repeat(72)}\n\n`;
  writeFileSync(
    out,
    `Third-party software bundled in the printscan web UI.${rule}${sections.join(rule)}\n`,
  );
  return out;
}

if (import.meta.main) {
  console.log(`wrote ${await writeLicenses(process.argv[2] ?? join(import.meta.dir, "../dist"))}`);
}
