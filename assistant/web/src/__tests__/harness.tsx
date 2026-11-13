// Shared test plumbing: a stubbed API, the real providers, and fixtures.
import type { ReactNode } from "react";
import { ChatProvider } from "../chat";
import { SessionProvider } from "../shell";
import { SpeakProvider } from "../speak";
import type { ActivityView, Request, Session } from "../types";

export const session = (over: Partial<Session> = {}): Session => ({
  Owner: "owner@example.com",
  Typed: true,
  Mic: true,
  Volume: true,
  Activity: true,
  CanSpeak: true,
  Speak: false,
  ...over,
});

export const emptyActivity = (requests: Request[] = []): ActivityView => ({
  Owner: "owner@example.com",
  Page: "activity",
  Error: "",
  Notice: "",
  Draft: "",
  HasActivity: true,
  HasTyped: true,
  DisplayLimit: 100,
  Just: null,
  Requests: requests,
  JustRequestID: "",
  PrivacyNotice: "",
  LatencyNotice: "",
  CollectionNote: "",
});

export const req = (over: Partial<Request> & { ID: string }): Request => ({
  Time: "",
  TimeISO: "2026-11-13T10:00:00",
  Source: "voice",
  SourceLabel: "Voice",
  Path: "agent_path",
  PathLabel: "Agent path",
  Tool: "",
  Outcome: "ok",
  OutcomeText: "OK",
  OutcomePill: "ok",
  Response: "900 ms",
  Measured: true,
  Error: "",
  Transcript: "hello",
  Reply: "hi",
  LocalOnly: false,
  Pending: false,
  Stages: [],
  Checkpoints: [],
  Calls: [],
  ...over,
});

const json = (body: unknown, status = 200) =>
  new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

export type Route = (init?: RequestInit) => Response | Promise<Response>;

/**
 * Installs a fetch that answers only the routes a test names, so an unexpected
 * request fails the test instead of reaching the network.
 */
export function stubFetch(routes: Record<string, Route>) {
  const fetch = async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input);
    const path = new URL(url, "http://localhost").pathname + new URL(url, "http://localhost").search;
    const route = routes[path] ?? routes[new URL(url, "http://localhost").pathname];
    if (!route) throw new Error(`unexpected fetch: ${init?.method ?? "GET"} ${path}`);
    return route(init);
  };
  globalThis.fetch = fetch as typeof fetch;
  return fetch;
}

export { json };

/** The real session and chat stores, over the test's fetch. */
export function Providers({ children }: { children: ReactNode }) {
  return (
    <SessionProvider>
      <SpeakProvider>
        <ChatProvider>{children}</ChatProvider>
      </SpeakProvider>
    </SessionProvider>
  );
}
