import { lazy, Suspense, useEffect } from "react";
import { useLocation } from "./api";
import { ChatProvider } from "./chat";
import { PageBoundary } from "./error-boundary";
import { LiveProvider } from "./events";
import { Home } from "./pages/Home";
import { pageLoaders, prefetchNear, type LazyPageId } from "./routes";
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
  lights: lazyPage("lights", "LightsPage"),
  monster: lazyPage("monster", "MonsterPage"),
  sounds: lazyPage("sounds", "SoundsPage"),
  voices: lazyPage("voices", "VoicesPage"),
  personas: lazyPage("personas", "PersonasPage"),
  phrasebook: lazyPage("phrasebook", "PhrasebookPage"),
  routes: lazyPage("routes", "RoutesPage"),
  hermes: lazyPage("hermes", "HermesPage"),
  "voice-samples": lazyPage("voice-samples", "VoiceSamplesPage"),
  spotify: lazyPage("spotify", "SpotifyPage"),
  secrets: lazyPage("secrets", "SecretsPage"),
  settings: lazyPage("settings", "SettingsPage"),
};

/**
 * Warm the pages either side of the current one once the page is interactive:
 * one prefetch per neighbour, not the every-route import this used to do.
 */
export function usePrefetchNear(current: PageId | undefined) {
  useEffect(() => {
    if (!current) return;
    const run = () => prefetchNear(current);
    if (typeof requestIdleCallback === "function") {
      const id = requestIdleCallback(run, { timeout: 4000 });
      return () => cancelIdleCallback(id);
    }
    const id = setTimeout(run, 1500);
    return () => clearTimeout(id);
  }, [current]);
}

export function App() {
  const { path } = useLocation();
  const page = pageFor(path);
  const View = page ? VIEWS[page.id] : NotFound;

  usePrefetchNear(page?.id);

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
              {/* The shell must survive a broken page: show what failed and offer a reload. */}
              {/* The page id key remounts PageBoundary on route navigation, resetting any render error. */}
              <PageBoundary key={page?.id} page={page?.id ?? "?"} title={page?.title}>
                <View key={page?.id} />
              </PageBoundary>
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
