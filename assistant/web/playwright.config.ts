// Browser smoke test: drives the production bundle through the Go dashboard
// handler (cmd/dashsmoke) so routing, CSP and assets are the ones the Pi ships.
import { defineConfig, devices } from "@playwright/test";

const PORT = 8099;
// The populated mode serves a fixture view per page so the route and axe suites
// exercise the real markup; it runs on a second port behind its own server.
const PORT_POPULATED = 8098;

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
    { name: "desktop", use: { ...devices["Desktop Chrome"], baseURL: `http://127.0.0.1:${PORT}` } },
    // The populated mode proves the route and axe suites see the real markup
    // (tables, editors, colour inputs, the completion-audio form) rather than
    // the first-run not-configured branch. It only runs those two suites; the
    // phone project keeps testing the first-run state as today.
    {
      name: "desktop-populated",
      use: { ...devices["Desktop Chrome"], baseURL: `http://127.0.0.1:${PORT_POPULATED}` },
      grep: /loads clean|has no axe violations/,
    },
    { name: "phone", use: { ...devices["Pixel 7"], baseURL: `http://127.0.0.1:${PORT}` } },
  ],
  webServer: [
    {
      // The stub stands in for the Pi: same handler and bundle, in-memory answers.
      command: `go run ./cmd/dashsmoke -addr 127.0.0.1:${PORT}`,
      cwd: "..",
      url: `http://127.0.0.1:${PORT}/api/ui/session`,
      reuseExistingServer: !process.env.CI,
      timeout: 120_000,
    },
    {
      // The populated mode serves a fixture view per page.
      command: `go run ./cmd/dashsmoke -addr 127.0.0.1:${PORT_POPULATED} -fixtures`,
      cwd: "..",
      url: `http://127.0.0.1:${PORT_POPULATED}/api/ui/session`,
      reuseExistingServer: !process.env.CI,
      timeout: 120_000,
    },
  ],
});
