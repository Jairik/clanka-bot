// The install list: the shell, Home, icons and fonts. A lazy route chunk must
// never appear, or a first visit pays for every page twice.
import { describe, expect, it } from "vitest";
import { precacheList } from "../precache";

const html = `<!doctype html>
<script src="/theme.js"></script>
<script type="module" crossorigin src="/assets/index-abc.js"></script>
<link rel="modulepreload" crossorigin href="/assets/rolldown-runtime-abc.js">
<link rel="modulepreload" crossorigin href="/assets/shell-abc.js">
<link rel="stylesheet" crossorigin href="/assets/index-abc.css">`;

const FILES = [
  "/index.html",
  "/offline.html",
  "/theme.js",
  "/manifest.webmanifest",
  "/favicon.svg",
  "/icons/apple-touch-icon.png",
  "/icons/icon-192.png",
  "/assets/index-abc.js",
  "/assets/index-abc.css",
  "/assets/rolldown-runtime-abc.js",
  "/assets/shell-abc.js",
  "/assets/hedvig-letters-sans-latin-400-normal-abc.woff2",
  "/assets/hedvig-letters-sans-latin-400-normal-abc.woff",
  // Lazy pages and the chunks they share must stay out.
  "/assets/Activity-abc.js",
  "/assets/Settings-abc.js",
  "/assets/VoiceSamples-abc.js",
  "/assets/forms-abc.js",
  "/assets/recorder-worklet-abc.js",
  "/assets/upload-abc.js",
];

describe("precacheList", () => {
  const list = precacheList(FILES, html);

  it("keeps the shell, Home, icons and fonts", () => {
    expect(list).toEqual([
      "/assets/hedvig-letters-sans-latin-400-normal-abc.woff",
      "/assets/hedvig-letters-sans-latin-400-normal-abc.woff2",
      "/assets/index-abc.css",
      "/assets/index-abc.js",
      "/assets/rolldown-runtime-abc.js",
      "/assets/shell-abc.js",
      "/favicon.svg",
      "/icons/apple-touch-icon.png",
      "/icons/icon-192.png",
      "/index.html",
      "/manifest.webmanifest",
      "/offline.html",
      "/theme.js",
    ]);
  });

  it("drops every lazy route chunk", () => {
    for (const file of ["/assets/Activity-abc.js", "/assets/Settings-abc.js", "/assets/VoiceSamples-abc.js"]) {
      expect(list).not.toContain(file);
    }
  });

  it("drops the chunks that only routes share", () => {
    for (const file of ["/assets/forms-abc.js", "/assets/recorder-worklet-abc.js", "/assets/upload-abc.js"]) {
      expect(list).not.toContain(file);
    }
  });

  it("never lists a compressed sibling", () => {
    expect(precacheList([...FILES, "/assets/index-abc.js.gz", "/assets/index-abc.js.br"], html)).toEqual(list);
  });
});
