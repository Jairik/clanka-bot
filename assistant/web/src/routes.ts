// The page chunk map. Home ships in the main bundle (it is the landing route);
// every other page is a dynamic import, and the shell can prefetch one on
// hover/focus or all of them once the app is idle.
import type { PageId } from "./shell";

export const pageLoaders = {
  activity: () => import("./pages/Activity"),
  alerts: () => import("./pages/Alerts"),
  health: () => import("./pages/Health"),
  lights: () => import("./pages/Lights"),
  notes: () => import("./pages/Notes"),
  personas: () => import("./pages/Personas"),
  secrets: () => import("./pages/Secrets"),
  settings: () => import("./pages/Settings"),
  sounds: () => import("./pages/Sounds"),
  spotify: () => import("./pages/Spotify"),
  "voice-samples": () => import("./pages/VoiceSamples"),
  voices: () => import("./pages/Voices"),
} as const;

export type LazyPageId = keyof typeof pageLoaders;

export const isLazy = (id: PageId): id is LazyPageId => id in pageLoaders;

/** Warm one page chunk; a failed prefetch is retried by the real navigation. */
export function prefetch(id: PageId) {
  if (isLazy(id)) void pageLoaders[id]().catch(() => {});
}

/** Warm every page chunk after first paint, so navigation feels instant. */
export function prefetchAll() {
  for (const load of Object.values(pageLoaders)) void load().catch(() => {});
}
