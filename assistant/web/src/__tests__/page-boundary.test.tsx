// PageBoundary isolates broken page views so the shell survives, and AppBoundary
// backstops the shell itself.
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { API_VERSION } from "../api-version";
import { AppBoundary, PageBoundary } from "../error-boundary";
import { Shell } from "../shell";
import { json, Providers, session, stubFetch } from "./harness";

function BrokenView({ message = "Broken view crashed" }: { message?: string }): React.JSX.Element {
  throw new Error(message);
}

function RecoverableView({ shouldThrow }: { shouldThrow: boolean }) {
  if (shouldThrow) {
    throw new Error("Temporary page render error");
  }
  return <div>Healthy page content</div>;
}

describe("PageBoundary", () => {
  beforeEach(() => {
    vi.restoreAllMocks();
  });

  it("renders page title, error message, API version, and a Reload button on error", async () => {
    const errorSpy = vi.spyOn(console, "error").mockImplementation(() => {});
    const reloadMock = vi.fn();

    render(
      <PageBoundary page="alerts" onReload={reloadMock}>
        <BrokenView message="Alerts exploded during render" />
      </PageBoundary>,
    );

    const alert = screen.getByRole("alert");
    expect(alert).toBeTruthy();
    // Page title from PAGES registry for "alerts" is "Alerts"
    expect(screen.getByRole("heading", { name: "Alerts" })).toBeTruthy();
    expect(screen.getByText("Alerts exploded during render")).toBeTruthy();
    expect(screen.getByText(`API v${API_VERSION}`)).toBeTruthy();

    const reloadButton = screen.getByRole("button", { name: "Reload" });
    expect(reloadButton).toBeTruthy();

    const user = userEvent.setup();
    await user.click(reloadButton);
    expect(reloadMock).toHaveBeenCalledTimes(1);

    // Logs once with the page id (React also logs an error banner in dev/test)
    const pageLogs = errorSpy.mock.calls.filter((c) =>
      typeof c[0] === "string" && c[0].includes('Page "alerts" render error:'),
    );
    expect(pageLogs).toHaveLength(1);
  });

  it("resets when its key changes so navigation clears the error", () => {
    vi.spyOn(console, "error").mockImplementation(() => {});

    // Initial render with key="alerts" throws
    const { rerender } = render(
      <PageBoundary key="alerts" page="alerts">
        <RecoverableView shouldThrow={true} />
      </PageBoundary>,
    );

    expect(screen.getByRole("alert")).toBeTruthy();
    expect(screen.getByText("Temporary page render error")).toBeTruthy();
    expect(screen.queryByText("Healthy page content")).toBeNull();

    // Changing the key to "notes" resets the boundary
    rerender(
      <PageBoundary key="notes" page="notes">
        <RecoverableView shouldThrow={false} />
      </PageBoundary>,
    );

    expect(screen.queryByRole("alert")).toBeNull();
    expect(screen.getByText("Healthy page content")).toBeTruthy();
  });

  it("resets on page prop change if reused without key change", () => {
    vi.spyOn(console, "error").mockImplementation(() => {});

    const { rerender } = render(
      <PageBoundary page="alerts">
        <RecoverableView shouldThrow={true} />
      </PageBoundary>,
    );

    expect(screen.getByRole("alert")).toBeTruthy();

    // Changing the page prop clears the error state
    rerender(
      <PageBoundary page="notes">
        <RecoverableView shouldThrow={false} />
      </PageBoundary>,
    );

    expect(screen.queryByRole("alert")).toBeNull();
    expect(screen.getByText("Healthy page content")).toBeTruthy();
  });

  it("keeps the shell (top bar and navigation) alive when a page crashes and recovers on navigation", async () => {
    vi.spyOn(console, "error").mockImplementation(() => {});
    stubFetch({ "/api/ui/session": () => json(session()) });

    let throwError = true;

    function TestShellApp({ currentPath }: { currentPath: string }) {
      const pageId = currentPath === "/alerts" ? "alerts" : "notes";
      return (
        <Providers>
          <Shell>
            <PageBoundary key={pageId} page={pageId}>
              {pageId === "alerts" && throwError ? (
                <BrokenView message="Alerts route failed" />
              ) : (
                <div data-testid="page-content">{pageId} content</div>
              )}
            </PageBoundary>
          </Shell>
        </Providers>
      );
    }

    const { rerender } = render(<TestShellApp currentPath="/alerts" />);

    // Fallback is rendered with page title and error message
    expect(await screen.findByRole("alert")).toBeTruthy();
    expect(screen.getByRole("heading", { name: "Alerts" })).toBeTruthy();
    expect(screen.getByText("Alerts route failed")).toBeTruthy();

    // The shell's top bar and navigation are still rendered and functional
    expect(screen.getByLabelText("Search activity")).toBeTruthy();
    expect(screen.getByLabelText("Notes")).toBeTruthy();
    expect(screen.getByLabelText("Alerts")).toBeTruthy();
    expect(screen.getByRole("navigation", { name: "Pages" })).toBeTruthy();

    // Navigate to Notes page
    throwError = false;
    rerender(<TestShellApp currentPath="/notes" />);

    // Boundary resets on key change and healthy page is rendered
    expect(screen.queryByRole("alert")).toBeNull();
    expect(screen.getByTestId("page-content").textContent).toBe("notes content");
  });
});

describe("AppBoundary", () => {
  beforeEach(() => {
    vi.restoreAllMocks();
  });

  it("catches root-level crashes and renders backstop fallback", async () => {
    const errorSpy = vi.spyOn(console, "error").mockImplementation(() => {});
    const reloadMock = vi.fn();

    render(
      <AppBoundary onReload={reloadMock}>
        <BrokenView message="Root shell collapsed" />
      </AppBoundary>,
    );

    expect(screen.getByRole("alert")).toBeTruthy();
    expect(screen.getByRole("heading", { name: "Dashboard crashed" })).toBeTruthy();
    expect(screen.getByText("Root shell collapsed")).toBeTruthy();
    expect(screen.getByText(`API v${API_VERSION}`)).toBeTruthy();

    const reloadButton = screen.getByRole("button", { name: "Reload" });
    const user = userEvent.setup();
    await user.click(reloadButton);
    expect(reloadMock).toHaveBeenCalledTimes(1);

    const rootLogs = errorSpy.mock.calls.filter((c) =>
      typeof c[0] === "string" && c[0].includes("Dashboard shell render error:"),
    );
    expect(rootLogs).toHaveLength(1);
  });
});
