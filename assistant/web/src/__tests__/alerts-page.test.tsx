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

it("renders retention settings on the alerts page", async () => {
  const setting = {
    Key: "alerts.history_days",
    Anchor: "setting-alerts.history_days",
    Label: "History days",
    Value: "90",
    Display: "90",
    Edit: "90",
    Source: "default",
    SourceLabel: "Default",
    LowerLabel: "code default",
    FormKind: "number",
    SaveAction: "/alerts/values/alerts.history_days",
    ResetAction: "/alerts/values/alerts.history_days/reset",
    Target: "#alerts-content",
    Restart: false,
    Override: false,
    Bool: false,
    Description: "Days of fired alert history to keep before deleting",
    PreviewAction: "",
    PreviewKind: "",
    Options: null,
  };
  const view = {
    HasManagers: true,
    Alerts: [],
    Alarms: [],
    Missed: [],
    ZoneName: "UTC",
    MissedLimit: 20,
    DesktopNotify: false,
    Settings: [setting],
  };
  stubFetch({
    "/api/ui/session": () => json(session()),
    "/api/ui/alerts": () => json(view),
  });
  render(<Providers><AlertsPage /></Providers>);
  expect(await screen.findByText("Retention")).toBeTruthy();
  expect(screen.getByText("History days")).toBeTruthy();
  expect(screen.getAllByText("90").length).toBeGreaterThan(0);
});
