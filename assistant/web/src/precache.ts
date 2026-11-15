// What the service worker downloads at install. The shell, Home, icons and
// fonts, never a lazy route chunk: a first visit should not pay for Settings,
// Voice Samples or Monster, and an unvisited deep link is not promised offline.
// Shared by the Vite plugin and its unit test.

/** Files that carry no content hash but are part of the shell. */
export const SHELL_FILES = [
  "/index.html",
  "/offline.html",
  "/theme.js",
  "/manifest.webmanifest",
  "/favicon.svg",
];

// Fonts keep their hashed names under assets/; everything else there is either
// the initial JS/CSS (referenced by index.html) or a lazy chunk.
const FONT = /^\/assets\/[^/]+\.(?:woff2?|ttf|otf)$/;

/** The files the worker precaches: the shell, Home, icons and fonts. */
export function precacheList(files: string[], html: string): string[] {
  // The entry chunk, the shell chunk and the modulepreload imports Vite writes
  // into index.html are exactly the code a first paint needs.
  const initial = new Set<string>();
  for (const match of html.matchAll(/(?:src|href)="(\/assets\/[^"]+\.(?:js|css))"/g)) {
    initial.add(match[1]);
  }
  return files
    .filter((file) => {
      if (SHELL_FILES.includes(file)) return true;
      if (file.startsWith("/icons/")) return true;
      if (initial.has(file)) return true;
      return FONT.test(file);
    })
    .sort();
}
