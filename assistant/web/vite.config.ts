import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";

// The build is embedded in the Go binary (internal/dashboard/ui/dist), so the
// Pi serves it with no CDN and no internet access.
//
// `pnpm dev` proxies data and write routes to a local assistant. The dashboard
// trusts only the Tailscale identity header from loopback, so the proxy adds
// it for the configured owner (DASHBOARD_OWNER, default "dev").
const backend = process.env.DASHBOARD_URL ?? "http://127.0.0.1:8082";
const owner = process.env.DASHBOARD_OWNER ?? "dev";

export default defineConfig({
  plugins: [react(), tailwindcss()],
  build: {
    outDir: "../internal/dashboard/ui/dist",
    emptyOutDir: true,
    // Fonts and icons stay as separate hashed files instead of data: URIs.
    assetsInlineLimit: 0,
  },
  server: {
    proxy: {
      // Page paths (/alerts) stay with Vite; their sub-routes are the API.
      "^/(api|activity|settings|secrets|sounds|voices|personas|voice-samples|alerts|spotify)/.*": {
        target: backend,
        headers: { "Tailscale-User-Login": owner },
        changeOrigin: true,
      },
    },
  },
});
