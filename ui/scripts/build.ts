// Bundles the UI into dist/ (embedded by ui/embed.go). dist/.keep always survives so Go builds without a UI build.
import {
  copyFileSync,
  existsSync,
  mkdirSync,
  readdirSync,
  readFileSync,
  rmSync,
  watch,
  writeFileSync,
} from "node:fs";
import { join, resolve } from "node:path";
import { writeLicenses } from "./licenses.ts";

const ui = join(import.meta.dir, "..");

const fontFiles = join(ui, "node_modules/@fontsource-variable/figtree/files");

/** Copies every font a built stylesheet references (url(./files/x.woff2)) next to that stylesheet. */
function copyFonts(outdir: string) {
  const assets = join(outdir, "assets");
  for (const css of readdirSync(assets).filter((f) => f.endsWith(".css"))) {
    for (const m of readFileSync(join(assets, css), "utf8").matchAll(
      /url\("?\.\/files\/([\w.-]+\.woff2)"?\)/g,
    )) {
      const name = m[1] as string;
      mkdirSync(join(assets, "files"), { recursive: true });
      copyFileSync(join(fontFiles, name), join(assets, "files", name));
    }
  }
}

export async function build(outdir = join(ui, "dist"), opts: { minify?: boolean } = {}) {
  outdir = resolve(outdir);
  mkdirSync(outdir, { recursive: true });
  for (const entry of readdirSync(outdir)) {
    if (entry !== ".keep") rmSync(join(outdir, entry), { recursive: true, force: true });
  }

  const result = await Bun.build({
    entrypoints: [join(ui, "index.html")],
    outdir,
    minify: opts.minify ?? true,
    splitting: true,
    // Absolute URLs: the SPA fallback serves index.html on nested paths too.
    publicPath: "/",
    sourcemap: "linked",
    // Bun inlines CSS url() fonts as data: URIs, which the CSP (default-src 'self') blocks.
    // Keep them external and copy the files next to the CSS below.
    external: ["*.woff2"],
    naming: {
      entry: "[dir]/[name].[ext]",
      chunk: "assets/[name]-[hash].[ext]",
      asset: "assets/[name]-[hash].[ext]",
    },
  });
  if (!result.success) {
    throw new AggregateError(result.logs, "UI build failed");
  }

  copyFonts(outdir);
  await writeLicenses(outdir, ui);
  if (!existsSync(join(outdir, ".keep"))) writeFileSync(join(outdir, ".keep"), "");
  return result;
}

if (import.meta.main) {
  await build();
  console.log("built dist/");
  if (process.argv.includes("--watch")) {
    let timer: ReturnType<typeof setTimeout> | undefined;
    watch(join(ui, "src"), { recursive: true }, () => {
      clearTimeout(timer);
      timer = setTimeout(() => {
        build(undefined, { minify: false }).then(
          () => console.log("rebuilt dist/"),
          (err) => console.error(err),
        );
      }, 100);
    });
    console.log("watching src/ for changes");
  }
}
