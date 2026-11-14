import { lazy, Suspense, useEffect } from "react";
import { useLocation } from "./api";
import { ChatProvider } from "./chat";
import { LiveProvider } from "./events";
import { Home } from "./pages/Home";
import { pageLoaders, prefetchAll, type LazyPageId } from "./routes";
import { SessionProvider, Shell, pageFor, type PageId } from "./shell";
import { SpeakProvider } from "./speak";

const lazyPage = <K extends LazyPageId>(id: K, name: string) =>
  lazy(async () => {
    const mod = (await pageLoaders[id]()) as Record<string, React.ComponentType>;
    return { default: mod[name] };
  });

// Each page is its own chunk; the shell (session, chat store, top bar, fonts)
// stays in the entry, and Home is eager because it is the landing route.
const VIEWS: Record<PageId, React.ComponentType> = {
  home: Home,
  activity: lazyPage("activity", "ActivityPage"),
  alerts: lazyPage("alerts", "AlertsPage"),
  notes: lazyPage("notes", "NotesPage"),
  health: lazyPage("health", "HealthPage"),
  sounds: lazyPage("sounds", "SoundsPage"),
  voices: lazyPage("voices", "VoicesPage"),
  personas: lazyPage("personas", "PersonasPage"),
  "voice-samples": lazyPage("voice-samples", "VoiceSamplesPage"),
  spotify: lazyPage("spotify", "SpotifyPage"),
  secrets: lazyPage("secrets", "SecretsPage"),
  settings: lazyPage("settings", "SettingsPage"),
};

export function App() {
  const { path } = useLocation();
  const page = pageFor(path);
  const View = page ? VIEWS[page.id] : NotFound;

  // Warm the rest of the routes once the landing page is interactive.
  useEffect(() => {
    const run = () => prefetchAll();
    if (typeof requestIdleCallback === "function") {
      const id = requestIdleCallback(run, { timeout: 4000 });
      return () => cancelIdleCallback(id);
    }
    const id = setTimeout(run, 1500);
    return () => clearTimeout(id);
  }, []);

  return (
    <SessionProvider>
      {/* The speak opt-in lives with the session: one value for the toggle and the composer. */}
      <SpeakProvider>
        {/* One live stream for every page; it pauses the polling fallbacks. */}
        <LiveProvider>
        {/* The chat store lives above the router so a pending request survives page changes. */}
        <ChatProvider>
          <Shell>
            <Suspense fallback={<div className="p-8 text-sm text-muted">Loading…</div>}>
              <View key={page?.id} />
            </Suspense>
          </Shell>
        </ChatProvider>
        </LiveProvider>
      </SpeakProvider>
    </SessionProvider>
  );
}

function NotFound() {
  return (
    <div className="flex h-full flex-col items-center justify-center gap-2 p-8 text-center">
      <h1 className="font-serif text-3xl">Not here</h1>
      <p className="text-sm text-muted">There is no page at this address.</p>
      <a href="/" className="text-sm underline">
        Go home
      </a>
    </div>
  );
}
