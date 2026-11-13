// Every route loads without a console error or a failed request, the chat
// answers, the mic reflects its states, and a refused volume write is shown.
// The same suite runs axe in both themes and walks the keyboard paths.
import AxeBuilder from "@axe-core/playwright";
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

test("a touch hold records and sends", async ({ page, isMobile, context }) => {
  test.skip(!isMobile, "touch gesture only");
  await page.goto("/");
  const talk = page.getByRole("button", { name: "Talk" });
  const box = await talk.boundingBox();
  if (!box) throw new Error("the mic button has no box");

  // A real touch hold: start, wait past the 250 ms minimum, release.
  const cdp = await context.newCDPSession(page);
  const point = { x: box.x + box.width / 2, y: box.y + box.height / 2 };
  await cdp.send("Input.dispatchTouchEvent", { type: "touchStart", touchPoints: [point] });
  await expect(page.getByRole("button", { name: "Cancel recording" })).toBeVisible();
  await page.waitForTimeout(400);
  await cdp.send("Input.dispatchTouchEvent", { type: "touchEnd", touchPoints: [] });

  // The stub keeps one activity ring, so earlier mic tests share the transcript.
  await expect(page.getByText("You said: hello from the browser").last()).toBeVisible();
});

test("page chunks are not part of the initial load", async ({ page, isMobile, request }) => {
  test.skip(isMobile, "the rail navigation is desktop-only");
  // The document the server sends is the initial load: it may name the entry
  // and the shell, never a route chunk.
  const html = await (await request.get("/")).text();
  expect(html).toMatch(/assets\/index-[^"]+\.js/);
  expect(html).not.toMatch(/Settings-|VoiceSamples-/);

  const chunks: string[] = [];
  page.on("response", (r) => {
    if (r.url().includes("/assets/") && r.url().endsWith(".js")) chunks.push(r.url());
  });
  await page.goto("/");
  await page.getByRole("link", { name: "Settings" }).click();
  await expect(page.getByRole("heading", { level: 1 })).toHaveText("Settings");
  expect(chunks.some((url) => /Settings-[^/]+\.js$/.test(url))).toBe(true);
});

test("installs: manifest, worker and an offline shell", async ({ page, context }) => {
  await page.goto("/");

  const manifest = await page.evaluate(async () => {
    const href = document.querySelector('link[rel="manifest"]')?.getAttribute("href") ?? "";
    const res = await fetch(href);
    return {
      status: res.status,
      type: res.headers.get("content-type") ?? "",
      body: (await res.json()) as { display?: string; icons?: unknown[] },
    };
  });
  expect(manifest.status).toBe(200);
  expect(manifest.type).toContain("application/manifest+json");
  expect(manifest.body.display).toBe("standalone");
  expect(manifest.body.icons?.length).toBeGreaterThan(1);

  // One reload lets the worker take control of the page it precached.
  await page.waitForFunction(async () => (await navigator.serviceWorker.getRegistrations()).length > 0);
  await page.evaluate(() => navigator.serviceWorker.ready);
  await page.reload();
  await page.waitForFunction(() => !!navigator.serviceWorker.controller);

  // The Pi is unreachable: the installed app opens and says so.
  await context.setOffline(true);
  await page.reload();
  await expect(page.getByText(/Could not reach the assistant/)).toBeVisible();
  await context.setOffline(false);
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

test.describe("accessibility", () => {
  for (const route of ROUTES) {
    for (const theme of ["light", "dark"] as const) {
      test(`${route} has no axe violations (${theme})`, async ({ page, isMobile }) => {
        test.skip(isMobile, "the desktop project scans both themes");
        await page.emulateMedia({ colorScheme: theme });
        await page.goto(route);
        await page.waitForLoadState("networkidle");
        const results = await new AxeBuilder({ page }).analyze();
        expect(results.violations).toEqual([]);
      });
    }
  }

  test("the skip link reaches the main content", async ({ page, isMobile }) => {
    test.skip(isMobile, "the desktop rail has the skip link");
    await page.goto("/");
    // Session loading can outlast navigation; send Tab only after the app mounts.
    await expect(page.getByRole("link", { name: "Skip to content" })).toBeAttached();
    await page.keyboard.press("Tab");
    const focused = page.locator(":focus");
    await expect(focused).toHaveText("Skip to content");
    await focused.press("Enter");
    await expect.poll(() => page.evaluate(() => document.activeElement?.id)).toBe("main");
  });

  test("a collapsible section opens with the keyboard", async ({ page }) => {
    await page.goto("/health");
    const trigger = page.getByRole("button", { name: /Compose services/ });
    const initial = (await trigger.getAttribute("aria-expanded")) === "true" ? "true" : "false";
    const flipped = initial === "true" ? "false" : "true";
    await trigger.focus();
    await trigger.press("Enter");
    await expect(trigger).toHaveAttribute("aria-expanded", flipped);
    await trigger.press("Space");
    await expect(trigger).toHaveAttribute("aria-expanded", initial);
  });

  test("the composer sends with Enter from the keyboard", async ({ page }, testInfo) => {
    await page.goto("/");
    const box = page.getByRole("textbox", { name: "Message" });
    await box.focus();
    const question = `keyboard ${testInfo.project.name} ${Date.now()}`;
    await page.keyboard.type(question);
    await page.keyboard.press("Enter");
    await expect(page.getByText(`You said: ${question}`)).toBeVisible();
  });

  test("the phone sheet takes focus and returns it on close", async ({ page, isMobile }) => {
    test.skip(!isMobile, "phone sheet only");
    await page.goto("/");
    const more = page.getByRole("navigation", { name: "Sections" }).getByRole("button", { name: "More" });
    await more.focus();
    await more.press("Enter");
    await expect(page.getByRole("heading", { name: "More" })).toBeVisible();
    // Focus moves into the dialog.
    await expect.poll(() => page.evaluate(() => !!document.activeElement?.closest("[role=dialog]"))).toBe(true);
    await page.keyboard.press("Escape");
    await expect(page.getByRole("heading", { name: "More" })).toBeHidden();
    await expect(more).toBeFocused();
  });
});
