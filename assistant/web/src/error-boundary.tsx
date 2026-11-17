// Error boundaries for the Dashboard: isolating page failures from the shell
// and catching root errors in the application.
import { Component, type ErrorInfo, type ReactNode } from "react";
import { Button } from "@heroui/react";
import { API_VERSION } from "./api-version";
import { PAGES } from "./shell";

export interface PageBoundaryProps {
  page: string;
  title?: string;
  onReload?: () => void;
  children: ReactNode;
}

interface PageBoundaryState {
  hasError: boolean;
  error: unknown;
}

// Catches render errors inside a page view so the surrounding Dashboard shell
// (navigation, top bar, volume, live stream, composer) remains functional.
export class PageBoundary extends Component<PageBoundaryProps, PageBoundaryState> {
  state: PageBoundaryState = {
    hasError: false,
    error: null,
  };

  static getDerivedStateFromError(error: unknown): PageBoundaryState {
    return { hasError: true, error };
  }

  componentDidCatch(error: unknown, errorInfo: ErrorInfo) {
    // Log once with the page id so user reports capture what failed and where.
    console.error(`Page "${this.props.page}" render error:`, error, errorInfo);
  }

  componentDidUpdate(prevProps: PageBoundaryProps) {
    // Navigating to another page clears the error state if the boundary instance was preserved.
    if (this.state.hasError && prevProps.page !== this.props.page) {
      this.setState({ hasError: false, error: null });
    }
  }

  private handleReload = () => {
    if (this.props.onReload) {
      this.props.onReload();
      return;
    }
    if (typeof window !== "undefined" && typeof window.location?.reload === "function") {
      try {
        window.location.reload();
      } catch {
        // Reload may fail in tests or restricted environments.
      }
    }
  };

  render() {
    if (!this.state.hasError) {
      return this.props.children;
    }

    const title =
      this.props.title ??
      PAGES.find((p) => p.id === this.props.page)?.title ??
      (this.props.page === "?" ? "Page" : this.props.page);

    const errorMessage =
      this.state.error instanceof Error
        ? this.state.error.message
        : String(this.state.error || "Unknown render error");

    return (
      <div role="alert" className="mx-auto max-w-xl space-y-4 p-8">
        <div className="space-y-1">
          <h1 className="font-serif text-2xl">{title}</h1>
          <p className="text-sm text-muted">This page encountered an error during render.</p>
        </div>
        <div className="rounded border bg-surface p-3 font-mono text-xs text-danger break-words">
          {errorMessage}
        </div>
        <div className="flex items-center gap-3 pt-2">
          <Button size="sm" variant="secondary" onPress={this.handleReload}>
            Reload
          </Button>
          <span className="text-xs text-muted">API v{API_VERSION}</span>
        </div>
      </div>
    );
  }
}

export interface AppBoundaryProps {
  onReload?: () => void;
  children: ReactNode;
}

interface AppBoundaryState {
  hasError: boolean;
  error: unknown;
}

// Top-level error boundary wrapping the entire application to catch crashes in the shell.
export class AppBoundary extends Component<AppBoundaryProps, AppBoundaryState> {
  state: AppBoundaryState = {
    hasError: false,
    error: null,
  };

  static getDerivedStateFromError(error: unknown): AppBoundaryState {
    return { hasError: true, error };
  }

  componentDidCatch(error: unknown, errorInfo: ErrorInfo) {
    console.error("Dashboard shell render error:", error, errorInfo);
  }

  private handleReload = () => {
    if (this.props.onReload) {
      this.props.onReload();
      return;
    }
    if (typeof window !== "undefined" && typeof window.location?.reload === "function") {
      try {
        window.location.reload();
      } catch {
        // Reload may fail in tests or restricted environments.
      }
    }
  };

  render() {
    if (!this.state.hasError) {
      return this.props.children;
    }

    const errorMessage =
      this.state.error instanceof Error
        ? this.state.error.message
        : String(this.state.error || "Unknown render error");

    return (
      <div role="alert" className="mx-auto max-w-xl space-y-4 p-8">
        <div className="space-y-1">
          <h1 className="font-serif text-2xl">Dashboard crashed</h1>
          <p className="text-sm text-muted">
            An unexpected error prevented the dashboard shell from rendering.
          </p>
        </div>
        <div className="rounded border bg-surface p-3 font-mono text-xs text-danger break-words">
          {errorMessage}
        </div>
        <div className="flex items-center gap-3 pt-2">
          <Button size="sm" variant="secondary" onPress={this.handleReload}>
            Reload
          </Button>
          <span className="text-xs text-muted">API v{API_VERSION}</span>
        </div>
      </div>
    );
  }
}
