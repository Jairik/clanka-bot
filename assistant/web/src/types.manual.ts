// Hand-written types with no Go counterpart. cmd/gentypes never rewrites this
// file: add a view-derived type to the Go structs instead.
import type {
  ActivityView,
  AlertsView,
  HealthView,
  HermesView,
  NotesView,
  PersonasView,
  PhrasebookView,
  RoutesView,
  SecretsView,
  SettingsView,
  SoundsView,
  SpotifyView,
  VoiceSamplesView,
  VoicesView,
} from "./types";

/** The answer every write returns: the refreshed view, or its error text. */
export type WriteResult<T> = { ok: boolean; status: number; view: T | null; text: string };

/** Every write answers with the refreshed view plus its flash or error. */
export type Flashable = { Flash?: string; Error?: string };

/** The page name each view belongs to; `usePageData` reads /api/ui/<page>. */
export type PageViews = {
  activity: ActivityView;
  "voice-samples": VoiceSamplesView;
  health: HealthView;
  settings: SettingsView;
  secrets: SecretsView;
  sounds: SoundsView;
  voices: VoicesView;
  personas: PersonasView;
  phrasebook: PhrasebookView;
  routes: RoutesView;
  hermes: HermesView;
  alerts: AlertsView;
  notes: NotesView;
  spotify: SpotifyView;
};
export type DataPage = keyof PageViews;
