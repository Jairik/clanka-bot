import { defineConfig, type Plugin } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";
import { brotliCompressSync, constants, gzipSync } from "node:zlib";
import { createHash } from "node:crypto";
import { readdirSync, readFileSync, statSync, writeFileSync } from "node:fs";
import { extname, join, relative } from "node:path";

// The build is embedded in the Go binary (internal/dashboard/ui/dist), so the
// Pi serves it with no CDN and no internet access.
//
// `pnpm dev` proxies data and write routes to a local assistant. The dashboard
// trusts only the Tailscale identity header from loopback, so the proxy adds
// it for the configured owner (DASHBOARD_OWNER, default "dev").
const backend = process.env.DASHBOARD_URL ?? "http://127.0.0.1:8082";
const owner = process.env.DASHBOARD_OWNER ?? "dev";

// Text assets get .gz and .br siblings at build time, so the Go handler serves
// a precompressed file instead of compressing on every request. Fonts and
// images are already compressed and are left alone.
const TEXT = new Set([".js", ".css", ".html", ".svg", ".json"]);
const MIN_BYTES = 512;

function precompress(): Plugin {
  let outDir = "";
  return {
    name: "clanka-precompress",
    apply: "build",
    configResolved(config) {
      outDir = config.build.outDir;
    },
    closeBundle() {
      const walk = (dir: string) => {
        for (const entry of readdirSync(dir, { withFileTypes: true })) {
          const file = join(dir, entry.name);
          if (entry.isDirectory()) {
            walk(file);
            continue;
          }
          if (!TEXT.has(extname(entry.name))) continue;
          const body = readFileSync(file);
          if (body.length < MIN_BYTES) continue;
          writeFileSync(`${file}.gz`, gzipSync(body, { level: 9 }));
          writeFileSync(`${file}.br`, brotliCompressSync(body, { params: { [constants.BROTLI_PARAM_QUALITY]: 11 } }));
        }
      };
      walk(outDir);
    },
  };
}

// The service worker precaches the shell; its file list and version come from
// the finished bundle, so a new build replaces the old cache.
function serviceWorker(): Plugin {
  let outDir = "";
  return {
    name: "clanka-service-worker",
    apply: "build",
    configResolved(config) {
      outDir = config.build.outDir;
    },
    closeBundle() {
      const files: string[] = [];
      const walk = (dir: string) => {
        for (const entry of readdirSync(dir, { withFileTypes: true })) {
          const file = join(dir, entry.name);
          if (entry.isDirectory()) {
            walk(file);
            continue;
          }
          // The SW itself and its compressed siblings are not part of the shell.
          if (entry.name === "sw.js" || /\.(gz|br)$/.test(entry.name)) continue;
          files.push("/" + relative(outDir, file).replaceAll("\\", "/"));
        }
      };
      walk(outDir);
      files.sort();

      const version = createHash("sha256")
        .update(files.map((f) => f + ":" + statSync(join(outDir, f.slice(1))).size).join("|"))
        .digest("hex")
        .slice(0, 12);
      const template = readFileSync("src/sw-template.js", "utf8");
      const sw = template.replace("__VERSION__", version).replace("__PRECACHE__", JSON.stringify(files, null, 2));
      writeFileSync(join(outDir, "sw.js"), sw);
    },
  };
}

export default defineConfig({
  plugins: [react(), tailwindcss(), precompress(), serviceWorker()],
  build: {
    outDir: "../internal/dashboard/ui/dist",
    emptyOutDir: true,
    // Fonts and icons stay as separate hashed files instead of data: URIs.
    assetsInlineLimit: 0,
  },
  server: {    proxy: {
      // Page paths (/alerts) stay with Vite; their sub-routes are the API.
      "^/(api|activity|settings|secrets|sounds|voices|personas|voice-samples|alerts|spotify)/.*": {
        target: backend,
        headers: { "Tailscale-User-Login": owner },
        changeOrigin: true,
      },
    },
  },
});
