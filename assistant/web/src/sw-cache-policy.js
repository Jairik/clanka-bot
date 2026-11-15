// The service worker's cache decision, kept as plain JavaScript because the
// build inlines this exact text into the worker (a classic worker cannot import
// a module) and the unit test imports the same file. Editing this file changes
// what the shipped worker caches.

// Shell files that carry no content hash but do carry the build's version: a
// new build gets a new cache name, so a cached copy is never stale within one.
const SHELL = new Set(["/theme.js", "/offline.html", "/manifest.webmanifest", "/favicon.svg"]);

/**
 * Cache-first is a decision, not a default: only immutable build output and the
 * versioned shell are cached.
 *
 * Everything else is network-only. Preview and take URLs vary by query string
 * and by the owner's settings (voice, rate, the draft text), media needs Range
 * and is private, and `/api/` reads are handled before this runs.
 *
 * @param {{ destination?: string }} request
 * @param {URL} url
 * @returns {boolean}
 */
export function shouldCache(request, url) {
  // Previews and takes put their whole identity in the query string, which the
  // cached key would then be keyed by while the bytes are still private.
  if (url.search) return false;
  // Media is served with Range and is the owner's own audio; never cache it.
  if (request.destination === "audio") return false;
  if (SHELL.has(url.pathname)) return true;
  return (
    url.pathname.startsWith("/assets/") ||
    url.pathname.startsWith("/icons/") ||
    url.pathname.startsWith("/fonts/")
  );
}
