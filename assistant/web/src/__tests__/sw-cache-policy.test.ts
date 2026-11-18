// The service worker's cache decision: only immutable build output (and the
// versioned shell) is cache-first. Previews, takes and media are not, because
// their URL does not carry the voice, the rate or the owner.
import { describe, expect, it } from "vitest";
import { bundleShellURLs, shouldCache } from "../sw-cache-policy.js";

const url = (path: string) => new URL(path, "https://clanka.example");
const get = (destination = "") => ({ destination });

describe("shouldCache", () => {
  it("caches content-hashed build output", () => {
    expect(shouldCache(get("script"), url("/assets/index-abc.js"))).toBe(true);
    expect(shouldCache(get("style"), url("/assets/index-abc.css"))).toBe(true);
    expect(shouldCache(get("font"), url("/assets/hedvig-letters-sans-abc.woff2"))).toBe(true);
    expect(shouldCache(get("image"), url("/icons/icon-192.png"))).toBe(true);
    expect(shouldCache(get("font"), url("/fonts/inter.woff2"))).toBe(true);
  });

  it("caches the versioned shell files", () => {
    expect(shouldCache(get("script"), url("/theme.js"))).toBe(true);
    expect(shouldCache(get(), url("/offline.html"))).toBe(true);
  });

  // Offline artifact URLs are cached without opening the cache to private queries.
  it("precaches and serves only the expected build-qualified shell files", () => {
    const digest = "a".repeat(64);
    const paths = bundleShellURLs(digest);
    expect(paths).toHaveLength(4);
    expect(paths).toContain("/theme.js?ui_build=" + digest);
    for (const path of paths) expect(shouldCache(get(), url(path))).toBe(true);
    expect(bundleShellURLs(null)).toEqual([]);
    expect(shouldCache(get(), url("/theme.js?ui_build=invalid"))).toBe(false);
    expect(shouldCache(get(), url("/voices/audio/alba?ui_build=" + digest))).toBe(false);
    expect(shouldCache(get(), url(paths[0] + "&extra=yes"))).toBe(false);
  });

  it("never caches a preview, whose query carries the draft text", () => {
    expect(shouldCache(get("audio"), url("/settings/wording/preview?text=one+moment"))).toBe(false);
    expect(shouldCache(get(), url("/settings/wording/preview?text=one+moment"))).toBe(false);
    expect(shouldCache(get("audio"), url("/voices/audio/alba?text=hello"))).toBe(false);
  });

  it("never caches a recorded take", () => {
    expect(shouldCache(get(), url("/voice-samples/take/clanker/positive/real_p_1"))).toBe(false);
    expect(shouldCache(get("audio"), url("/voice-samples/take/clanker/positive/real_p_1"))).toBe(false);
  });

  it("never caches sound or voice audio", () => {
    expect(shouldCache(get("audio"), url("/sounds/audio/birds"))).toBe(false);
    expect(shouldCache(get("audio"), url("/voices/audio/alba"))).toBe(false);
  });

  it("never caches a reply's audio: it is the owner's words in the active voice", () => {
    expect(shouldCache(get(), url("/reply/audio/req-1"))).toBe(false);
    expect(shouldCache(get("audio"), url("/reply/audio/req-1"))).toBe(false);
  });

  it("never caches any media request, whatever its path", () => {
    expect(shouldCache(get("audio"), url("/assets/theme.wav"))).toBe(false);
  });

  it("leaves API reads and ordinary routes to the network", () => {
    expect(shouldCache(get(), url("/api/ui/session"))).toBe(false);
    expect(shouldCache(get(), url("/settings"))).toBe(false);
    expect(shouldCache(get("document"), url("/index.html"))).toBe(false);
  });
});
