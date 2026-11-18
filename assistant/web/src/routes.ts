// The page chunk map. Home ships in the main bundle (it is the landing route);
// every other page is a dynamic import, and the shell warms one on hover/focus
// or the page either side of the current one once the app is idle. A first
// visit never downloads the pages it did not open.
import type { PageId } from "./shell";

export const pageLoaders = {
  activity: () => import("./pages/Activity"),
  alerts: () => import("./pages/Alerts"),
  health: () => import("./pages/Health"),
  lights: () => import("./pages/Lights"),
  monster: () => import("./pages/Monster"),
  notes: () => import("./pages/Notes"),
  personas: () => import("./pages/Personas"),
  routes: () => import("./pages/Routes"),
  hermes: () => import("./pages/Hermes"),
  secrets: () => import("./pages/Secrets"),
  settings: () => import("./pages/Settings"),
  sounds: () => import("./pages/Sounds"),
  spotify: () => import("./pages/Spotify"),
  "voice-samples": () => import("./pages/VoiceSamples"),
  voices: () => import("./pages/Voices"),
} as const;

export type LazyPageId = keyof typeof pageLoaders;

export const isLazy = (id: PageId): id is LazyPageId => id in pageLoaders;

// The rail order the shell renders, so "near" means the pages either side of
// the one the user is on. Kept here because the shell imports this module; a
// unit test pins it to PAGES so the two cannot drift.
export const railOrder: PageId[] = [
  "home",
  "alerts",
  "notes",
  "voices",
  "personas",
  "sounds",
  "spotify",
  "lights",
  "monster",
  "activity",
  "health",
  "routes",
  "voice-samples",
  "secrets",
  "hermes",
  "settings",
];

/** Warm one page chunk; a failed prefetch is retried by the real navigation. */
export function prefetch(id: PageId) {
  if (isLazy(id)) void pageLoaders[id]().catch(() => {});
}

type Connection = { saveData?: boolean; effectiveType?: string };

const connection = (): Connection | undefined =>
  typeof navigator === "undefined"
    ? undefined
    : (navigator as Navigator & { connection?: Connection }).connection;

/** saveData users and slow links get the page they asked for, nothing else. */
export function cheapLink(net: Connection | undefined = connection()): boolean {
  if (!net) return true;
  if (net.saveData) return false;
  return !/2g/.test(net.effectiveType ?? "");
}

/**
 * Warm the pages either side of this one after first paint. The old
 * prefetchAll() imported every route, so a phone on the tailnet downloaded
 * Settings, Voice Samples and Monster before the user asked for any of them.
 */
export function prefetchNear(current: PageId | null | undefined, load: (id: PageId) => void = prefetch) {
  if (!current || !cheapLink()) return;
  const at = railOrder.indexOf(current);
  if (at < 0) return;
  for (const id of [railOrder[at - 1], railOrder[at + 1]]) if (id) load(id);
}
