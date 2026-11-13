// DOM tests use the real React components and stub only device/network boundaries.
import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";
export default defineConfig({ plugins: [react()], test: { environment: "jsdom", setupFiles: ["./src/test-setup.ts"] } });
