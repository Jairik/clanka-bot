// Scheduling completion media posts the media together with the timer duration.
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { expect, it } from "vitest";
import { AlertsPage } from "../pages/Alerts";
import { json, Providers, session, stubFetch } from "./harness";

it("schedules a timer with completion audio", async () => {
  let body = "";
  const view = { HasManagers: true, Alerts: [], Alarms: [], Missed: [], ZoneName: "UTC", MissedLimit: 20 };
  stubFetch({
    "/api/ui/session": () => json(session()),
    "/api/ui/alerts": () => json(view),
    "/alerts/create": (init) => { body = String(init?.body); return json(view); },
  });
  const user = userEvent.setup();
  render(<Providers><AlertsPage /></Providers>);
  await user.type(await screen.findByRole("textbox", { name: "Completion audio" }), "playlist: Running");
  await user.click(screen.getByRole("button", { name: "Schedule" }));
  expect(new URLSearchParams(body).get("audio")).toBe("playlist: Running");
  expect(new URLSearchParams(body).get("seconds")).toBe("900");
});
