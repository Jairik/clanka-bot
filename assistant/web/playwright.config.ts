// Browser smoke test: drives the production bundle through the Go dashboard
// handler (cmd/dashsmoke) so routing, CSP and assets are the ones the Pi ships.
import { defineConfig, devices } from "@playwright/test";

const PORT = 8099;

export default defineConfig({
  testDir: "./e2e",
  timeout: 30_000,
  fullyParallel: true,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  reporter: process.env.CI ? [["list"], ["github"]] : "list",
  use: {
    baseURL: `http://127.0.0.1:${PORT}`,
    trace: "retain-on-failure",
    permissions: ["microphone"],
    launchOptions: {
      // A synthetic microphone lets the recording state be exercised in CI.
      args: ["--use-fake-device-for-media-stream", "--use-fake-ui-for-media-stream"],
    },
  },
  projects: [
    { name: "desktop", use: { ...devices["Desktop Chrome"] } },
    { name: "phone", use: { ...devices["Pixel 7"] } },
  ],
  webServer: {
    // The stub stands in for the Pi: same handler and bundle, in-memory answers.
    command: `go run ./cmd/dashsmoke -addr 127.0.0.1:${PORT}`,
    cwd: "..",
    url: `http://127.0.0.1:${PORT}/api/ui/session`,
    reuseExistingServer: !process.env.CI,
    timeout: 120_000,
  },
});
