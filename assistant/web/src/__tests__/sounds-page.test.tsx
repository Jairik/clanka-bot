// The Sounds page installs a YouTube link into the same library a file upload refreshes.
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { expect, it } from "vitest";
import { SoundsPage } from "../pages/Sounds";
import type { SoundsView } from "../types";
import { json, Providers, session, stubFetch } from "./harness";

const library = (over: Partial<SoundsView> = {}): SoundsView => ({
  Owner: "owner@example.com",
  Page: "sounds",
  Flash: "",
  Error: "",
  HasLibrary: true,
  MaxUpload: "10 MB",
  Sounds: [],
  Roles: [],
  HasRoles: false,
  RingKinds: [],
  HasRing: false,
  ...over,
});

it("installs a YouTube URL and refreshes the library", async () => {
  let body = "";
  const installed = library({
    Flash: "Installed. The sound is in the library and ready to assign.",
    Sounds: [
      {
        Name: "Birds-at-Dawn",
        Duration: "1s",
        Added: "20 Nov 2026",
        Origin: "Uploaded",
        AudioURL: "/sounds/audio/Birds-at-Dawn",
        DeleteAction: "/sounds/delete/Birds-at-Dawn",
      },
    ],
  });
  stubFetch({
    "/api/ui/session": () => json(session()),
    "/api/ui/sounds": () => json(library()),
    "/sounds/youtube": (init) => {
      body = String(init?.body);
      return json(installed);
    },
  });
  const user = userEvent.setup();
  render(
    <Providers>
      <SoundsPage />
    </Providers>,
  );
  const field = await screen.findByRole("textbox", { name: "YouTube URL" });
  await user.type(field, "https://www.youtube.com/watch?v=abc123");
  await user.click(screen.getByRole("button", { name: "Install from YouTube" }));
  expect(new URLSearchParams(body).get("url")).toBe("https://www.youtube.com/watch?v=abc123");
  expect(await screen.findByText("Birds-at-Dawn")).toBeTruthy();
  expect(await screen.findByText(/Installed\./)).toBeTruthy();
  expect((field as HTMLInputElement).value).toBe("");
});

it("shows the server error when a YouTube install is refused", async () => {
  stubFetch({
    "/api/ui/session": () => json(session()),
    "/api/ui/sounds": () => json(library()),
    "/sounds/youtube": () => json(library({ Error: "Enter a public YouTube video link." }), 400),
  });
  const user = userEvent.setup();
  render(
    <Providers>
      <SoundsPage />
    </Providers>,
  );
  const field = await screen.findByRole("textbox", { name: "YouTube URL" });
  await user.type(field, "https://example.com/nope");
  await user.click(screen.getByRole("button", { name: "Install from YouTube" }));
  expect(await screen.findByRole("alert")).toHaveProperty("textContent", expect.stringContaining("Enter a public YouTube video link."));
  expect((field as HTMLInputElement).value).toBe("https://example.com/nope");
});
