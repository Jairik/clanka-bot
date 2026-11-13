// Every route loads without a console error or a failed request, the chat
// answers, the mic reflects its states, and a refused volume write is shown.
import { expect, test, type Page } from "@playwright/test";

const ROUTES = [
  "/",
  "/activity",
  "/alerts",
  "/notes",
  "/health",
  "/sounds",
  "/voices",
  "/personas",
  "/voice-samples",
  "/spotify",
  "/secrets",
  "/settings",
];

/** Records everything that would make a page broken for the user. */
function watch(page: Page) {
  const problems: string[] = [];
  page.on("console", (m) => {
    if (m.type() === "error") problems.push(`console: ${m.text()}`);
  });
  page.on("pageerror", (e) => problems.push(`pageerror: ${e.message}`));
  page.on("requestfailed", (r) => problems.push(`request failed: ${r.url()} ${r.failure()?.errorText ?? ""}`));
  page.on("response", (r) => {
    if (r.status() >= 400) problems.push(`response ${r.status()}: ${r.url()}`);
  });
  return problems;
}

test.describe("routes", () => {
  for (const route of ROUTES) {
    test(`${route} loads clean`, async ({ page }) => {
      const problems = watch(page);
      await page.goto(route);
      await expect(page.getByRole("heading", { level: 1 })).toBeVisible();
      await page.waitForLoadState("networkidle");
      expect(problems).toEqual([]);
    });
  }
});

test("the collector catches a broken page", async ({ page }) => {
  const problems = watch(page);
  // A deliberately broken API route: the test above must not pass a page whose
  // data never arrived.
  await page.route("**/api/ui/health*", (r) => r.abort());
  await page.goto("/health");
  await expect.poll(() => problems.length).toBeGreaterThan(0);
});

test("the chat answers a typed request", async ({ page }, testInfo) => {
  await page.goto("/");
  // The stub keeps one activity ring for the whole run, so each test asks its
  // own question instead of sharing one answer.
  const question = `what time is it ${testInfo.project.name} ${Date.now()}`;
  const box = page.getByRole("textbox", { name: "Message" });
  await box.fill(question);
  await box.press("Enter");
  await expect(page.getByText(`You said: ${question}`)).toBeVisible();
});

test("the mic reaches its recording state and can be cancelled", async ({ page }) => {
  await page.goto("/");
  const talk = page.getByRole("button", { name: "Talk" });
  await expect(talk).toBeEnabled();
  await talk.click();
  await expect(page.getByRole("button", { name: "Cancel recording" })).toBeVisible();
  await page.getByRole("button", { name: "Cancel recording" }).click();
  await expect(page.getByRole("button", { name: "Talk" })).toBeVisible();
});

test("the app and its script arrive precompressed", async ({ page, request }) => {
  await page.goto("/");
  const doc = await request.get("/", { headers: { "Accept-Encoding": "br" } });
  expect(doc.headers()["content-encoding"]).toBe("br");

  const src = await page.evaluate(() => document.querySelector('script[type="module"]')?.getAttribute("src") ?? "");
  const path = new URL(src, "http://127.0.0.1:8099").pathname;
  const plain = await request.get(path, { headers: { "Accept-Encoding": "identity" } });
  const br = await request.get(path, { headers: { "Accept-Encoding": "br" } });
  expect(br.headers()["content-encoding"]).toBe("br");
  const plainBytes = Number(plain.headers()["content-length"]);
  const brBytes = Number(br.headers()["content-length"]);
  expect(plainBytes).toBeGreaterThan(0);
  expect(brBytes).toBeLessThan(plainBytes / 2);
});

test("a refused volume write shows the server's text", async ({ page }) => {
  await page.goto("/");
  await page.getByRole("button", { name: "Mute speaker" }).click();
  await expect(page.getByRole("status")).toContainText("The speaker did not accept that change.");
});

test("the phone layout opens the More sheet", async ({ page, isMobile }) => {
  test.skip(!isMobile, "phone layout only");
  await page.goto("/");
  const tabs = page.getByRole("navigation", { name: "Sections" });
  await tabs.getByRole("button", { name: "More" }).click();
  await expect(page.getByRole("heading", { name: "More" })).toBeVisible();
  await page.getByRole("button", { name: "Settings" }).click();
  await expect(page).toHaveURL(/\/settings$/);
  await expect(page.getByRole("heading", { level: 1 })).toBeVisible();
});
