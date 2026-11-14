// Home: an overview of summary cards over the prompt panel. Sending anything
// flips to the conversation; the History button flips back.
import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { Button, ToggleButton, ToggleButtonGroup } from "@heroui/react";
import { AlarmClock, CloudSun, Gauge, HeartPulse, History, KeyRound, Music, ScrollText, Timer, Volume2, Zap } from "lucide-react";
import { getJSON, navigate, useQueryParam } from "../api";
import { latestDelta, useLive, type LiveName } from "../events";
import { Composer, Exchange, PendingExchange, fmtMS, isFailed, isTyped, parseMS, totalMS, useChat } from "../chat";
import { useSession } from "../shell";
import type { AlertsView, HealthView, SecretsView, SpotifyView } from "../types";
import { Dot, cn, Tip } from "../ui";

const greeting = () => {
  const h = new Date().getHours();
  return h < 5 ? "Evening" : h < 12 ? "Morning" : h < 18 ? "Afternoon" : "Evening";
};

export function Home() {
  const [view, setView] = useQueryParam("view", "overview");
  const { Owner } = useSession();
  const { requests, pending } = useChat();
  const end = useRef<HTMLDivElement>(null);
  const chat = view === "chat";
  useEffect(() => {
    document.title = "clanka";
  }, []);
  useEffect(() => {
    if (chat) end.current?.scrollIntoView({ block: "end" });
  }, [requests.length, pending.length, chat]);
  const name = Owner.split("@")[0];

  return (
    <div className="flex h-full flex-col">
      <div className="min-h-0 flex-1 overflow-y-auto">
        <div className="mx-auto max-w-[1400px] px-4 pt-6 md:px-8 md:pt-10">
          <div className="flex flex-wrap items-end justify-between gap-4">
            <div>
              <h1 className="font-serif text-3xl tracking-tight md:text-[34px]">
                {greeting()} <span className="text-muted">{name}</span>
              </h1>
              <p className="mt-1 text-sm text-muted">here's a quick look at what clanka has been up to.</p>
            </div>
            {/* Toggle buttons rather than Tabs: the two views share one page
                and there is no tab panel to point aria-controls at. */}
            <ToggleButtonGroup
              selectionMode="single"
              disallowEmptySelection
              selectedKeys={new Set([chat ? "chat" : "overview"])}
              onSelectionChange={(keys) => setView(String([...keys][0]))}
              aria-label="Home view"
              className="border"
            >
              <ToggleButton id="overview" className="px-4">
                Overview
              </ToggleButton>
              <ToggleButton id="chat" className="px-4">
                Conversation
              </ToggleButton>
            </ToggleButtonGroup>
          </div>

          {chat ? (
            <div className="mx-auto max-w-3xl space-y-6 py-8" aria-live="polite">
              {requests.length === 0 && pending.length === 0 && (
                <p className="py-10 text-center text-sm text-muted">Nothing yet. Ask something below, or talk to the Pi.</p>
              )}
              {requests.slice(-20).map((r) => (
                <Exchange key={r.ID} r={r} />
              ))}
              {pending.map((p) => (
                <PendingExchange key={p.id} p={p} />
              ))}
              <div ref={end} />
            </div>
          ) : (
            <Overview />
          )}
        </div>
      </div>

      <div className="mx-auto w-full max-w-3xl shrink-0 px-3 pt-2 pb-3 md:px-4 md:pb-6">
        {!chat && <QuickActions onSent={() => setView("chat")} />}
        <Composer
          placeholder="Ask clanka to…"
          onSent={() => setView("chat")}
          toolbar={
            <Tip>
              <Button isIconOnly size="sm" variant="ghost" aria-label={chat ? "Show overview" : "Show conversation"} onPress={() => setView(chat ? "overview" : "chat")}>
                <History className="size-4" />
              </Button>
            </Tip>
          }
        />
      </div>
    </div>
  );
}

/** Rich text: segments wrapped in ** are foreground, the rest muted. */
function Say({ children }: { children: string }) {
  return (
    <p className="text-[15px] leading-snug text-muted">
      {children.split("**").map((t, i) => (i % 2 ? <span key={i} className="text-foreground">{t}</span> : t))}
    </p>
  );
}

function Card({
  icon: I, label, children, footer, to, className, right,
}: {
  icon?: typeof Gauge;
  label: string;
  children: ReactNode;
  footer?: ReactNode;
  to?: string;
  className?: string;
  right?: ReactNode;
}) {
  const body = (
    <>
      <div className="flex items-center gap-2 text-xs text-muted">
        {I && <I className="size-3.5" strokeWidth={1.5} />}
        {label}
        {right && <span className="ml-auto">{right}</span>}
      </div>
      <div className="mt-4 flex min-h-0 flex-1 flex-col">{children}</div>
      {footer && <div className="mt-3 text-xs text-subtle transition-colors group-hover:text-muted">{footer}</div>}
    </>
  );
  const cls = cn(
    "group flex h-[210px] w-[82vw] max-w-[340px] shrink-0 snap-start flex-col border bg-surface p-4 text-left md:w-auto md:max-w-none",
    to && "cursor-pointer transition-colors hover:bg-surface-secondary",
    className,
  );
  return to ? (
    <a
      href={to}
      className={cls}
      onClick={(e) => {
        if (e.button !== 0 || e.metaKey || e.ctrlKey) return;
        e.preventDefault();
        navigate(to);
      }}
    >
      {body}
    </a>
  ) : (
    <div className={cls}>{body}</div>
  );
}

function usePolled<T>(path: string, every = 60_000, name?: LiveName) {
  const [v, setV] = useState<T | null>(null);
  const { live, generation, subscribe, resync } = useLive();
  const load = useCallback(
    // Health and Alerts arrive as a whole view, so a delta that lands while the
    // snapshot is in flight is the newer state (#149).
    () =>
      resync(
        name ?? null,
        () => getJSON<T>(path),
        latestDelta,
      ).then(setV, () => {}),
    [path, name, resync],
  );
  useEffect(() => {
    void load();
    // Health and Alerts change over the stream; the rest keep their poll.
    if (live && name) return;
    const id = setInterval(() => document.visibilityState === "visible" && load(), every);
    return () => clearInterval(id);
  }, [every, live, name, load]);
  useEffect(() => {
    if (generation > 0) void load();
  }, [generation, load]);
  useEffect(() => {
    if (!name) return;
    return subscribe(name, (data) => setV(data as T));
  }, [name, subscribe]);
  return v;
}

const plural = (n: number, one: string, many = `${one}s`) => `${n} ${n === 1 ? one : many}`;
const median = (xs: number[]) => {
  if (!xs.length) return 0;
  const s = [...xs].sort((a, b) => a - b);
  return s[Math.floor(s.length / 2)];
};

function Overview() {
  const { requests } = useChat();
  const health = usePolled<HealthView>("/api/ui/health", 60_000, "health");
  const alerts = usePolled<AlertsView>("/api/ui/alerts", 30_000, "alerts");
  const spotify = usePolled<SpotifyView>("/api/ui/spotify");
  const secrets = usePolled<SecretsView>("/api/ui/secrets");

  const today = useMemo(() => {
    const start = new Date();
    start.setHours(0, 0, 0, 0);
    return requests.filter((r) => new Date(r.TimeISO) >= start);
  }, [requests]);
  const failed = today.filter(isFailed).length;
  const voice = today.filter((r) => !isTyped(r.Source)).length;
  const byHour = useMemo(() => {
    const now = new Date().getHours();
    return Array.from({ length: 12 }, (_, i) => {
      const h = (now - 11 + i + 24) % 24;
      const rs = today.filter((r) => new Date(r.TimeISO).getHours() === h);
      return { n: rs.length, bad: rs.some(isFailed) };
    });
  }, [today]);
  const max = Math.max(1, ...byHour.map((b) => b.n));
  const measured = requests.filter((r) => r.Measured).map((r) => parseMS(r.Response));
  const agent = requests.filter((r) => r.Path === "agent_path").slice(-14).map(totalMS);
  const amax = Math.max(1, ...agent);

  // Only checks that actually answer count: a module that declares no health
  // check is neither ok nor failing. unchecked is reported separately so the
  // number cannot read as "everything is wired" when 15 modules are not.
  const allChecks = health ? [...(health.Services ?? []), health.Location, ...(health.Modules ?? [])].filter((c) => !!c) : [];
  const checks = allChecks.filter((c) => c.Status !== "not-configured" && c.Status !== "no-check");
  const unchecked = allChecks.filter((c) => c.Status === "no-check").length;
  const down = checks.filter((c) => c.Status === "error" || c.Status === "timed-out");
  const next = alerts?.Alerts?.[0];
  const unset = (secrets?.Secrets ?? []).filter((s) => !s.Set);

  return (
    <div className="no-scrollbar -mx-4 mt-8 flex snap-x snap-mandatory gap-3 overflow-x-auto px-4 pb-2 md:mx-0 md:grid md:grid-cols-2 md:gap-4 md:overflow-visible md:px-0 xl:grid-cols-4">
      <div className="relative shrink-0 snap-start">
        <div className="absolute inset-0 translate-x-1 translate-y-1 border bg-surface" aria-hidden />
        <div className="absolute inset-0 translate-x-0.5 translate-y-0.5 border bg-surface" aria-hidden />
        <Card label="Today so far" className="relative bg-surface-secondary">
          <Say>
            {today.length
              ? `**${plural(today.length, "request")}**, **${today.length - failed} answered**, **${failed} failed**.` +
                (!health ? "" : down.length ? ` **${down.map((d) => d.Name).join(", ")}** ${down.length === 1 ? "is" : "are"} down.` : " Everything is **healthy**.")
              : "No requests yet today." + (down.length ? ` **${down.map((d) => d.Name).join(", ")}** ${down.length === 1 ? "is" : "are"} down.` : "")}
          </Say>
        </Card>
      </div>

      <Card icon={ScrollText} label="Activity" footer="See activity" to="/activity">
        <Say>{today.length ? `**${voice} of ${today.length}** requests came in **by voice** today` : "Nothing has reached the assistant **today**"}</Say>
        <div className="mt-auto flex h-12 items-end gap-1.5" aria-hidden>
          {byHour.map((b, i) => (
            <span
              key={i}
              className={cn("w-2.5", b.bad ? "bg-danger" : i === byHour.length - 1 ? "bg-foreground" : "bg-foreground/35")}
              style={{ height: `${b.n ? Math.max(8, (b.n / max) * 100) : 4}%` }}
            />
          ))}
        </div>
      </Card>

      <Card icon={HeartPulse} label="Health" footer="See health" to="/health" right={health && <Dot tone={down.length ? "bad" : "ok"} pulse={down.length > 0} />}>
        <Say>
          {!health
            ? "Checking…"
            : down.length
              ? `**${down[0].Name}**: ${down[0].Detail || down[0].StatusLabel}.` + (down.length > 1 ? ` **${down.length - 1} more** need attention.` : "")
              : unchecked
                ? "Every check that is wired is **healthy**."
                : "Every service and module check is **healthy**."}
        </Say>
        {health && (
          <div className="mt-auto flex flex-wrap items-baseline gap-x-2">
            <span className="text-3xl tabular-nums">
              {checks.length - down.length}
              <span className="text-muted">/{checks.length}</span>
            </span>
            {unchecked > 0 && <span className="text-xs text-muted">{unchecked} no check</span>}
          </div>
        )}
      </Card>

      <Card icon={AlarmClock} label="Alerts" footer="See alerts" to="/alerts">
        <Say>
          {!alerts
            ? "Loading…"
            : !alerts.HasManagers
              ? "Scheduled alerts are **not configured**."
              : next
                ? `**${next.Label || next.Kind}** (${next.Kind.toLowerCase()}) is next.` + ((alerts.Alerts?.length ?? 0) > 1 ? ` **${alerts.Alerts!.length - 1} more** scheduled.` : "")
                : "No **timers**, **alarms** or **reminders** are set."}
        </Say>
        {next && <div className="mt-auto truncate text-2xl tabular-nums">{next.Next}</div>}
      </Card>

      <Card icon={Gauge} label="Response time" footer="See activity" to="/activity">
        <Say>{measured.length ? `Median from **end of speech** to **first audio** over the last ${plural(measured.length, "voice reply", "voice replies")}` : "No measured **voice replies** yet. Typed requests have no speech to time."}</Say>
        {measured.length > 0 && <div className="mt-auto text-3xl tabular-nums">{fmtMS(median(measured))}</div>}
      </Card>

      <Card icon={Music} label="Spotify" footer="Open Spotify" to="/spotify" right={spotify && <Dot tone={spotify.Pairing.Pill === "ok" && spotify.WebAPI.Pill === "ok" ? "ok" : spotify.Pairing.Pill === "error" || spotify.WebAPI.Pill === "error" ? "bad" : "neutral"} />}>
        <Say>{spotify ? `Connect is **${spotify.Pairing.Label}**${spotify.Pairing.Paired && spotify.Pairing.Device ? ` as **${spotify.Pairing.Device}**` : ""}. Web API is **${spotify.WebAPI.Label}**.` : "Loading…"}</Say>
      </Card>

      <Card icon={KeyRound} label="Secrets" footer="Manage secrets" to="/secrets" right={unset.length > 0 && <Dot tone="warn" />}>
        <Say>
          {!secrets
            ? "Loading…"
            : !secrets.HasSecrets
              ? "Secrets are **not configured**."
              : unset.length
                ? `**${unset.slice(0, 2).map((s) => s.Name).join(", ")}**${unset.length > 2 ? ` and ${unset.length - 2} more` : ""} ${unset.length === 1 ? "is" : "are"} **not set**.`
                : `All **${plural(secrets.Secrets?.length ?? 0, "secret")}** are set.`}
        </Say>
        {secrets?.HasSecrets && (
          <div className="mt-auto text-3xl tabular-nums">
            {(secrets.Secrets?.length ?? 0) - unset.length}
            <span className="text-muted">/{secrets.Secrets?.length ?? 0}</span>
          </div>
        )}
      </Card>

      <Card icon={Zap} label="Agent path" footer="See activity" to="/activity">
        <Say>{agent.length ? `Total stage time of the last **${plural(agent.length, "Agent request")}**` : "No **Agent path** requests in recent history."}</Say>
        {agent.length > 1 && (
          <svg viewBox="0 0 130 40" className="mt-auto h-12 w-full" preserveAspectRatio="none" aria-hidden>
            <polyline
              fill="none"
              stroke="currentColor"
              strokeWidth="1.5"
              vectorEffect="non-scaling-stroke"
              points={agent.map((v, i) => `${(i / (agent.length - 1)) * 130},${38 - (v / amax) * 34}`).join(" ")}
            />
          </svg>
        )}
      </Card>
    </div>
  );
}

const QUICK = [
  { icon: CloudSun, label: "Weather", say: "what's the weather like tomorrow morning" },
  { icon: Timer, label: "Set a timer", say: "set a timer for 10 minutes" },
  { icon: Music, label: "Play music", say: "play some music" },
  { icon: Volume2, label: "Turn it down", say: "turn it down" },
  { icon: HeartPulse, label: "Health check", say: "is everything healthy" },
];
const SUGGEST = ["remind me to stretch in an hour", "set an alarm for 7 tomorrow", "what's on right now"];

function QuickActions({ onSent }: { onSent: () => void }) {
  const { send } = useChat();
  const { Typed } = useSession();
  if (!Typed) return null;
  const go = (t: string) => {
    send(t);
    onSent();
  };
  return (
    <>
      <div className="no-scrollbar -mx-3 mb-3 flex gap-2 overflow-x-auto px-3 md:mx-0 md:flex-wrap md:justify-center md:px-0">
        {QUICK.map(({ icon: I, label, say }) => (
          <button key={label} onClick={() => go(say)} className="flex h-9 shrink-0 items-center gap-2 border px-3 text-[13px] hover:bg-surface-secondary">
            <I className="size-3.5 text-muted" strokeWidth={1.5} />
            {label}
          </button>
        ))}
      </div>
      <div className="mb-1 hidden border bg-surface-secondary/60 py-1 md:block">
        {SUGGEST.map((s) => (
          <button key={s} onClick={() => go(s)} className="block w-full px-4 py-2.5 text-left text-sm hover:bg-default">
            Ask clanka to <span className="text-muted">{s}</span>
          </button>
        ))}
      </div>
    </>
  );
}
