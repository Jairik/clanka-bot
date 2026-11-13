import { useLocation } from "./api";
import { ChatProvider } from "./chat";
import { ActivityPage } from "./pages/Activity";
import { AlertsPage } from "./pages/Alerts";
import { HealthPage } from "./pages/Health";
import { Home } from "./pages/Home";
import { PersonasPage } from "./pages/Personas";
import { SecretsPage } from "./pages/Secrets";
import { SettingsPage } from "./pages/Settings";
import { SoundsPage } from "./pages/Sounds";
import { SpotifyPage } from "./pages/Spotify";
import { VoicesPage } from "./pages/Voices";
import { SessionProvider, Shell, pageFor, type PageId } from "./shell";

const VIEWS: Record<PageId, () => React.JSX.Element> = {
  home: Home,
  activity: ActivityPage,
  alerts: AlertsPage,
  health: HealthPage,
  sounds: SoundsPage,
  voices: VoicesPage,
  personas: PersonasPage,
  spotify: SpotifyPage,
  secrets: SecretsPage,
  settings: SettingsPage,
};

export function App() {
  const { path } = useLocation();
  const page = pageFor(path);
  const View = page ? VIEWS[page.id] : NotFound;
  return (
    <SessionProvider>
      {/* The chat store lives above the router so a pending request survives page changes. */}
      <ChatProvider>
        <Shell>
          <View key={page?.id} />
        </Shell>
      </ChatProvider>
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
