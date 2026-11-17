import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import "./index.css";
import { App } from "./App";
import { AppBoundary } from "./error-boundary";

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <AppBoundary>
      <App />
    </AppBoundary>
  </StrictMode>,
);

// The production build emits sw.js; the dev server has no shell to precache.
if (import.meta.env.PROD && "serviceWorker" in navigator) {
  void navigator.serviceWorker.register("/sw.js").catch(() => {
    // A browser without service workers (or a private window) still runs the app.
  });
}
