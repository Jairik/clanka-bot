// App frame: icon rail (desktop) / bottom tabs (mobile), a search-first top bar
// with the speaker volume, and the page area. Home (/) is the chat.
import { createContext, useCallback, useContext, useEffect, useRef, useState, type ReactNode } from "react";
import { Button, Drawer, Popover, SearchField } from "@heroui/react";
import {
  Activity, AlarmClock, Bell, HeartPulse, KeyRound, MessageSquare, Mic, Minus, MoreHorizontal, Music, Plus,
  Settings, Smile, StickyNote, Volume, Volume1, Volume2, VolumeX, Waves, type LucideIcon,
} from "lucide-react";
import { APIVersionMismatch, getJSON, navigate, postForm, useLocation } from "./api";
import { API_VERSION } from "./api-version";
import { latestDelta, useLive } from "./events";
import type { HealthView, Session, Volume as VolumeState } from "./types";
import { Range, Tip, cn } from "./ui";
import { SpeakToggle } from "./speak";
import { prefetch } from "./routes";
import { snapVolume, VOL_STEP } from "./volume";
import { Logo } from "./logo";

export type PageId = "home" | "activity" | "alerts" | "notes" | "health" | "sounds" | "voices" | "personas" | "voice-samples" | "spotify" | "secrets" | "settings";

export const PAGES: { id: PageId; path: string; title: string; blurb: string; icon: LucideIcon }[] = [
  { id: "home", path: "/", title: "Chat", blurb: "", icon: MessageSquare },
  { id: "activity", path: "/activity", title: "Activity", blurb: "Every request that reached the assistant, newest first.", icon: Activity },
  { id: "alerts", path: "/alerts", title: "Alerts", blurb: "Timers, Alarms and Reminders, plus the Rings nobody answered.", icon: AlarmClock },
  { id: "notes", path: "/notes", title: "Notes", blurb: "The shopping and to-do lists, and the facts you asked clanka to remember.", icon: StickyNote },
  { id: "health", path: "/health", title: "Health", blurb: "Each Compose service, the location, and every Tool module check.", icon: HeartPulse },
  { id: "sounds", path: "/sounds", title: "Sounds", blurb: "The sound library, Cue roles and Ring behaviour.", icon: Bell },
  { id: "voices", path: "/voices", title: "Voices", blurb: "The speaking voice, its rate, and the voice library.", icon: Waves },
  { id: "personas", path: "/personas", title: "Personas", blurb: "Bundles of voice, personality and wording, applied in one step.", icon: Smile },
  { id: "voice-samples", path: "/voice-samples", title: "Voice samples", blurb: "Record takes for a wake phrase, then export the set for desktop training.", icon: Mic },
  { id: "spotify", path: "/spotify", title: "Spotify", blurb: "Connect pairing for playback and Web API auth for control.", icon: Music },
  { id: "secrets", path: "/secrets", title: "Secrets", blurb: "Write-only values the Agent and Tool modules read.", icon: KeyRound },
  { id: "settings", path: "/settings", title: "Settings", blurb: "Tools, wording, quiet hours and every other live setting.", icon: Settings },
];
export const pageFor = (path: string) => PAGES.find((p) => p.path === path.replace(/\/+$/, "") || (p.path === "/" && path === "/"));

// ---------- session (who, and which optional services are wired) ----------

const SessionCtx = createContext<Session>({
  Owner: "",
  Typed: false,
  Mic: false,
  Volume: false,
  Activity: false,
  CanSpeak: false,
  Speak: false,
  APIVersion: API_VERSION,
});
export const useSession = () => useContext(SessionCtx);

// The UI and the assistant ship as separate artifacts, so a page can be older
// or newer than the server it reaches. Rendering anyway would show missing or
// renamed fields; fail loudly and say which versions met.
function VersionMismatch({ served }: { served: number }) {
  return (
    <div role="alert" className="mx-auto max-w-xl space-y-3 p-8">
      <h1 className="font-serif text-2xl">Dashboard and assistant versions do not match</h1>
      <p className="text-sm">
        This Dashboard build expects API v{API_VERSION}; the assistant at this address serves API v{served}.
      </p>
      <p className="text-sm text-muted">
        Deploy the <code className="font-mono">dashboard-web</code> and <code className="font-mono">assistant</code> images from the
        same commit, or roll one back to the version the other expects.
      </p>
    </div>
  );
}

export function SessionProvider({ children }: { children: ReactNode }) {
  const [s, setS] = useState<Session | null>(null);
  const [err, setErr] = useState("");
  const [mismatch, setMismatch] = useState(0);
  useEffect(() => {
    getJSON<Session>("/api/ui/session").then(setS, (e) => {
      // The response header is checked before the body, so this is the first
      // place a mismatched assistant is caught.
      if (e instanceof APIVersionMismatch) setMismatch(e.served);
      else setErr(String(e?.message ?? e));
    });
  }, []);
  if (mismatch) return <VersionMismatch served={mismatch} />;
  if (err) return <div className="p-8 text-sm text-danger">Could not reach the assistant: {err}</div>;
  if (!s) return null;
  if (s.APIVersion !== API_VERSION) return <VersionMismatch served={s.APIVersion} />;
  return <SessionCtx.Provider value={s}>{children}</SessionCtx.Provider>;
}

const healthIsBad = (h: HealthView) =>
  [...(h.Services ?? []), ...(h.Modules ?? []), h.Location].some((c) => c && (c.Status === "error" || c.Status === "timed-out"));

/** A red dot on Health when any check fails. The stream carries the change; polling is the fallback. */
function useHealthAlarm() {
  const [bad, setBad] = useState(false);
  const { live, generation, subscribe, resync } = useLive();
  const load = useCallback(
    // A health delta is a whole view, so one that arrives while the snapshot is
    // in flight is the newer state (#149).
    () =>
      resync("health", () => getJSON<HealthView>("/api/ui/health"), latestDelta).then(
        (h) => setBad(healthIsBad(h)),
        () => {},
      ),
    [resync],
  );
  useEffect(() => {
    void load();
    if (live) return;
    const id = setInterval(load, 60_000);
    return () => clearInterval(id);
  }, [load, live]);
  useEffect(() => {
    if (generation > 0) void load();
  }, [generation, load]);
  useEffect(() => subscribe("health", (data) => setBad(healthIsBad(data as HealthView))), [subscribe]);
  return bad;
}

export function Shell({ children }: { children: ReactNode }) {
  const { path } = useLocation();
  const page = pageFor(path)?.id;
  const healthBad = useHealthAlarm();
  const { live } = useLive();
  return (
    // data-live is the one observable seam for the browser smoke test: it says
    // whether the stream is carrying updates or the polling fallback is.
    <div className="flex h-dvh" data-live={live ? "live" : "polling"}>
      <a
        href="#main"
        className="sr-only focus:not-sr-only focus:absolute focus:top-2 focus:left-2 focus:z-50 focus:border focus:bg-surface focus:px-3 focus:py-2 focus:text-sm"
      >
        Skip to content
      </a>
      <aside className="hidden w-[70px] shrink-0 flex-col border-r md:flex">
        <Tip label="clanka" hint="Back to the chat" placement="right">
          <a href="/" onClick={link("/")} className="flex h-[70px] shrink-0 items-center justify-center border-b" aria-label="Home">
            <Logo className="size-6" />
          </a>
        </Tip>
        <nav className="flex flex-1 flex-col items-center gap-1 overflow-y-auto py-4" aria-label="Pages">
          {PAGES.map(({ id, path: to, title, blurb, icon: I }) => (
            // The link is the tooltip trigger itself (through Focusable), so
            // no button role wraps it. Hover/focus also warms that page's
            // chunk so navigation is instant.
            <Tip key={id} label={title} hint={id === "health" && healthBad ? "A check is failing." : blurb || "Talk to clanka."} placement="right">
              <a
                href={to}
                onClick={link(to)}
                onMouseEnter={() => prefetch(id)}
                onFocus={() => prefetch(id)}
                aria-label={title}
                aria-current={page === id ? "page" : undefined}
                className={cn(
                  "relative flex size-10 items-center justify-center border border-transparent",
                  page === id ? "border-border bg-surface-secondary text-foreground" : "text-muted hover:text-foreground",
                )}
              >
                <I className="size-[18px]" strokeWidth={1.5} />
                {id === "health" && healthBad && <span className="absolute top-2 right-2 size-1.5 rounded-full bg-danger" />}
              </a>
            </Tip>
          ))}
        </nav>
        <Owner />
      </aside>

      <div className="flex min-w-0 flex-1 flex-col">
        <header className="pt-safe flex h-14 shrink-0 items-center gap-2 border-b px-3 md:h-[70px] md:px-6">
          <a href="/" onClick={link("/")} className="flex size-9 items-center justify-center md:hidden" aria-label="Home">
            <Logo className="size-5" />
          </a>
          <form
            className="min-w-0 flex-1"
            onSubmit={(e) => {
              e.preventDefault();
              const q = String(new FormData(e.currentTarget).get("q") ?? "").trim();
              navigate(q ? `/activity?q=${encodeURIComponent(q)}` : "/activity");
            }}
          >
            <SearchField aria-label="Search activity" name="q" className="max-w-md">
              <SearchField.Group className="border-0 bg-transparent shadow-none">
                <SearchField.SearchIcon />
                <SearchField.Input placeholder="Find anything" />
              </SearchField.Group>
            </SearchField>
          </form>
          <SpeakToggle />
          <VolumeControl />
        </header>
        <main id="main" tabIndex={-1} className="min-h-0 flex-1 outline-none">
          {children}
        </main>
        <BottomTabs page={page} healthBad={healthBad} />
      </div>
    </div>
  );
}

/** Client-side navigation for a plain <a>, keeping middle-click/new-tab working. */
export const link = (to: string) => (e: React.MouseEvent) => {
  if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
  e.preventDefault();
  navigate(to);
};

function Owner() {
  const { Owner: owner } = useSession();
  return (
    <div className="flex h-[70px] shrink-0 items-center justify-center border-t">
      <Tip label="Signed in" hint={owner} placement="right">
        <span tabIndex={0} aria-label={`Signed in as ${owner}`} className="flex size-9 items-center justify-center border text-[11px] uppercase">
          {owner.slice(0, 2)}
        </span>
      </Tip>
    </div>
  );
}

/** Standard page frame: serif title and blurb, then the page body. */
export function PageFrame({ id, children, right }: { id: PageId; children: ReactNode; right?: ReactNode }) {
  const p = PAGES.find((x) => x.id === id)!;
  useEffect(() => {
    document.title = `${p.title} · clanka`;
  }, [p.title]);
  return (
    <div className="h-full overflow-y-auto">
      <div className="mx-auto max-w-4xl px-4 pt-6 pb-28 md:px-8 md:pt-10 md:pb-16">
        <div className="mb-6 flex flex-wrap items-end justify-between gap-3">
          <div>
            <h1 className="font-serif text-3xl tracking-tight">{p.title}</h1>
            <p className="mt-1 text-sm text-muted">{p.blurb}</p>
          </div>
          {right}
        </div>
        {children}
      </div>
    </div>
  );
}

// ---------- volume ----------

/**
 * The Pi speaker. The icon toggles mute (Pulse mute, so the level is kept); the
 * slider sets the level in 5% steps. Slider moves are coalesced so a drag is
 * one write per settle, not one per pixel.
 */
export function VolumeControl() {
  const { Volume: wired } = useSession();
  const [state, setState] = useState<VolumeState | null>(null);
  const [level, setLevel] = useState(0);
  const [error, setError] = useState("");
  const timer = useRef<ReturnType<typeof setTimeout>>(undefined);
  const dragging = useRef(false);
  // The last value the server reported. A write that never reaches it must put
  // the control back here: the slider is optimistic, and showing a level the
  // speaker never took is worse than showing the old one.
  const lastServer = useRef<VolumeState | null>(null);
  const { live, generation, subscribe, resync } = useLive();

  const load = useCallback(
    // A volume delta that arrives while the snapshot is in flight is newer than
    // the snapshot, so it is applied after it (#149).
    () =>
      resync("volume", () => getJSON<VolumeState>("/api/ui/volume"), latestDelta).then(
        (v) => {
          lastServer.current = v;
          setState(v);
          if (!dragging.current) setLevel(v.Percent);
          setError(v.Error || "");
        },
        () => {},
      ),
    [resync],
  );
  useEffect(() => {
    if (!wired) return;
    void load();
    // Voice commands change the volume too; while the stream is open it
    // carries that change, so the bar does not poll.
    if (live) return;
    const id = setInterval(() => document.visibilityState === "visible" && load(), 15_000);
    return () => clearInterval(id);
  }, [wired, live, load]);
  // A reconnect reloads the level, so a missed delta cannot leave a stale bar.
  useEffect(() => {
    if (wired && generation > 0) void load();
  }, [wired, generation, load]);
  useEffect(() => {
    if (!wired) return;
    return subscribe("volume", (data) => {
      const v = data as VolumeState;
      lastServer.current = v;
      setState(v);
      if (!dragging.current) setLevel(v.Percent);
      setError(v.Error || "");
    });
  }, [wired, subscribe]);

  if (!wired) return null;

  // A refused write answers 502 with the error text and often no level at
  // all. Showing it is the difference between "the speaker did not take it"
  // and a control that silently does nothing.
  if (!state?.Available) {
    return error ? (
      <span className="max-w-40 truncate text-[11px] text-danger" role="status">
        {error}
      </span>
    ) : null;
  }

  const write = async (body: Record<string, string>) => {
    try {
      const res = await postForm<VolumeState>("/api/ui/volume", body);
      if (res.view) {
        lastServer.current = res.view;
        setState(res.view);
        if (!dragging.current) setLevel(res.view.Percent);
      }
      setError(res.ok ? "" : res.view?.Error || res.text || "The speaker did not accept that change.");
    } catch {
      // A dropped request throws out of fetch instead of answering. Say so the
      // same way a refusal is said, and restore the last value the server
      // reported so the slider and the mute icon stay truthful.
      setError("Couldn't reach the speaker.");
      const last = lastServer.current;
      if (last) {
        setState(last);
        setLevel(last.Percent);
      }
    }
  };
  const setTo = (v: number) => {
    const c = snapVolume(v);
    setLevel(c);
    setState((s) => (s ? { ...s, Muted: c === 0 ? s.Muted : false } : s));
    dragging.current = true;
    clearTimeout(timer.current);
    timer.current = setTimeout(() => {
      dragging.current = false;
      void write({ percent: String(c) });
    }, 250);
  };
  const toggle = () => void write({ muted: String(!state.Muted) });

  const muted = state.Muted;
  const eff = muted ? 0 : level;
  const Icon = eff === 0 ? VolumeX : eff < 34 ? Volume : eff < 67 ? Volume1 : Volume2;
  const label = muted ? "Muted" : `${level}%`;
  // A read that timed out answers with the last level the speaker gave (#160).
  // That is a guess about now, not a failure: the control stays usable and the
  // treatment is a dimmed number plus one mark, so no colour and no new width.
  const stale = state.Stale;
  const staleHint = "The speaker has not answered just now; this is the last level it reported.";

  return (
    <div className="flex items-center gap-1">
      <Tip hint={stale ? staleHint : muted ? `The Pi speaker is muted at ${level}%.` : `The Pi speaker is at ${level}%.`}>
        <Button
          isIconOnly
          variant="ghost"
          onPress={toggle}
          aria-label={muted ? "Unmute speaker" : "Mute speaker"}
          aria-description={stale ? staleHint : undefined}
          aria-pressed={muted}
        >
          <Icon className={cn("size-5", muted && "text-danger")} strokeWidth={1.5} />
        </Button>
      </Tip>
      <div className="hidden w-28 px-1 sm:block">
        <Range label="Speaker volume" value={eff} onChange={setTo} step={VOL_STEP} />
      </div>
      <span
        title={stale ? staleHint : undefined}
        className={cn("hidden w-12 text-right font-mono text-xs tabular-nums sm:inline", stale ? "text-subtle" : "text-muted")}
      >
        {label}
        {/* The mark's width is reserved in both states so the digits never move. */}
        <span aria-hidden className="inline-block w-2">
          {stale ? "?" : ""}
        </span>
        {stale && <span className="sr-only">, last level the speaker reported</span>}
      </span>
      {error && (
        <span className="max-w-40 truncate text-[11px] text-danger" role="status">
          {error}
        </span>
      )}
      <Popover>
        <Button
          size="sm"
          variant="ghost"
          className={cn("font-mono text-xs tabular-nums sm:hidden", stale ? "text-subtle" : "text-muted")}
          aria-label={stale ? `Volume ${label}, last level the speaker reported` : `Volume ${label}`}
        >
          {label}
        </Button>
        <Popover.Content placement="bottom end" className="w-[min(18rem,calc(100vw-1.5rem))]">
          <Popover.Dialog className="p-4">
            <div className="mb-3 flex items-center justify-between">
              <Popover.Heading className="text-sm">Speaker</Popover.Heading>
              <span className={cn("font-mono text-sm tabular-nums", stale && "text-subtle")}>{label}</span>
            </div>
            {stale && <p className="mb-3 text-[11px] text-subtle">{staleHint}</p>}
            <div className="flex items-center gap-2">
              <Tip>
                <Button isIconOnly variant="outline" onPress={() => setTo(eff - VOL_STEP)} aria-label="Down 5%">
                  <Minus className="size-4" />
                </Button>
              </Tip>
              <Range label="Speaker volume" value={eff} onChange={setTo} step={VOL_STEP} />
              <Tip>
                <Button isIconOnly variant="outline" onPress={() => setTo(eff + VOL_STEP)} aria-label="Up 5%">
                  <Plus className="size-4" />
                </Button>
              </Tip>
            </div>
            <Button fullWidth variant={muted ? "primary" : "secondary"} className="mt-3" onPress={toggle}>
              {muted ? "Unmute" : "Mute"}
            </Button>
          </Popover.Dialog>
        </Popover.Content>
      </Popover>
    </div>
  );
}

// ---------- mobile bottom tabs + "More" sheet ----------

const TABS: PageId[] = ["home", "activity", "alerts"];

function BottomTabs({ page, healthBad }: { page?: PageId; healthBad: boolean }) {
  const [more, setMore] = useState(false);
  const inMore = !!page && !TABS.includes(page);
  const current = PAGES.find((p) => p.id === page);
  const tab = (key: string, Icon: LucideIcon, label: string, active: boolean, onClick: () => void, dot?: boolean) => (
    <button
      key={key}
      onClick={onClick}
      aria-current={active ? "page" : undefined}
      className={cn("flex flex-1 flex-col items-center justify-center gap-0.5 pt-2 pb-1.5 text-[11px]", active ? "text-foreground" : "text-muted")}
    >
      <span className={cn("relative flex h-7 w-12 items-center justify-center rounded-full transition-colors", active && "bg-default")}>
        <Icon className="size-5" strokeWidth={1.5} />
        {dot && <span className="absolute top-0.5 right-3 size-1.5 rounded-full bg-danger" />}
      </span>
      {label}
    </button>
  );
  return (
    <>
      <nav className="pb-safe flex border-t bg-surface/95 backdrop-blur md:hidden" aria-label="Sections">
        {TABS.map((id) => {
          const p = PAGES.find((x) => x.id === id)!;
          return tab(id, p.icon, p.title, page === id, () => navigate(p.path));
        })}
        {tab("more", MoreHorizontal, inMore && current ? current.title : "More", inMore, () => setMore(true), healthBad && page !== "health")}
      </nav>
      <Drawer>
        <Drawer.Backdrop isOpen={more} onOpenChange={setMore}>
          <Drawer.Content placement="bottom">
            <Drawer.Dialog className="pb-safe">
              <Drawer.Handle />
              <Drawer.Header>
                <Drawer.Heading className="text-sm">More</Drawer.Heading>
              </Drawer.Header>
              <Drawer.Body>
                <div className="grid grid-cols-3 gap-2">
                  {PAGES.filter((p) => !TABS.includes(p.id)).map(({ id, path: to, title, icon: I }) => (
                    <button
                      key={id}
                      onClick={() => {
                        navigate(to);
                        setMore(false);
                      }}
                      className={cn(
                        "relative flex flex-col items-center gap-1.5 border py-3.5 text-xs",
                        page === id ? "border-foreground/30 bg-default" : "hover:bg-surface-secondary",
                      )}
                    >
                      <I className="size-5" strokeWidth={1.5} />
                      {title}
                      {id === "health" && healthBad && <span className="absolute top-2 right-2 size-1.5 rounded-full bg-danger" />}
                    </button>
                  ))}
                </div>
              </Drawer.Body>
            </Drawer.Dialog>
          </Drawer.Content>
        </Drawer.Backdrop>
      </Drawer>
    </>
  );
}
