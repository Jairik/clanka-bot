// The conversation: one shared, polled view of the Activity store, the
// composer (typed + browser mic), and how one request renders. Voice requests
// made at the Pi appear here too, so the chat is the whole recent history.
import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { Button, Tooltip } from "@heroui/react";
import { ArrowUp, AudioLines, Bot, ChevronDown, Keyboard, Lock, Mic, MonitorSmartphone, RotateCcw, Square, X } from "lucide-react";
import { getJSON, postForm } from "./api";
import { isTypedSource } from "./activity";
import { MIC_MAX_SECONDS, micSupported } from "./mic";
import { useRecorder } from "./recorder";
import { useSession } from "./shell";
import { SpeakHint } from "./speak";
import type { ActivityView, Request } from "./types";
import { Badge, Fold, cn, foldOpen, type Tone } from "./ui";

// ---------- store ----------

export type PendingAsk = {
  id: string;
  source: "dashboard" | "dashboard_mic";
  text: string;
  at: Date;
  error?: string;
};

type Chat = {
  view: ActivityView | null;
  /** Oldest first. */
  requests: Request[];
  pending: PendingAsk[];
  loadError: string;
  busy: boolean;
  send: (text: string) => void;
  sendAudio: (pcm: ArrayBuffer) => void;
  retry: (p: PendingAsk) => void;
  dismiss: (id: string) => void;
};
const ChatCtx = createContext<Chat>(null!);
export const useChat = () => useContext(ChatCtx);

const POLL_MS = 4000;

/**
 * Pending-request ids. The clock alone is not enough: two sends in the same
 * millisecond would share an id, and settle/dismiss/retry act on one id.
 */
export const createRequestId = (clock: () => number = Date.now) => {
  let seq = 0;
  return () => `p${clock()}-${(seq += 1)}`;
};

export function ChatProvider({ children }: { children: ReactNode }) {
  const [view, setView] = useState<ActivityView | null>(null);
  const [pending, setPending] = useState<PendingAsk[]>([]);
  const [loadError, setLoadError] = useState("");

  const load = useCallback(async () => {
    try {
      setView(await getJSON<ActivityView>("/api/ui/activity"));
      setLoadError("");
    } catch (e) {
      setLoadError(e instanceof Error ? e.message : String(e));
    }
  }, []);
  useEffect(() => {
    void load();
    const id = setInterval(() => document.visibilityState === "visible" && load(), POLL_MS);
    const onVis = () => document.visibilityState === "visible" && load();
    document.addEventListener("visibilitychange", onVis);
    return () => {
      clearInterval(id);
      document.removeEventListener("visibilitychange", onVis);
    };
  }, [load]);

  const settle = (id: string, res: { ok: boolean; view: ActivityView | null; text: string; status: number }) => {
    if (res.view) setView(res.view);
    if (res.ok) setPending((p) => p.filter((x) => x.id !== id));
    else {
      const error = res.view?.Error || res.text || `Request failed (${res.status}).`;
      setPending((p) => p.map((x) => (x.id === id ? { ...x, error, text: x.text || res.view?.Draft || "" } : x)));
    }
  };
  const fail = (id: string, e: unknown) =>
    setPending((p) => p.map((x) => (x.id === id ? { ...x, error: e instanceof Error ? e.message : String(e) } : x)));

  // Two sends in the same millisecond must not share an id: settle, dismiss
  // and retry act on one id, and React needs unique keys.
  const nextId = useRef(createRequestId()).current;

  const send = useCallback((text: string) => {
    const id = nextId();
    setPending((p) => [...p, { id, source: "dashboard", text, at: new Date() }]);
    postForm<ActivityView>("/activity/ask", { text }).then((r) => settle(id, r), (e) => fail(id, e));
  }, [nextId]);

  const audio = useRef(new Map<string, ArrayBuffer>());
  const sendAudio = useCallback((pcm: ArrayBuffer) => {
    const id = nextId();
    audio.current.set(id, pcm);
    setPending((p) => [...p, { id, source: "dashboard_mic", text: "", at: new Date() }]);
    postForm<ActivityView>("/api/ui/ask/audio", undefined, {
      body: pcm,
      headers: { Accept: "application/json", "Content-Type": "audio/L16; rate=16000; channels=1; endianness=little-endian" },
    }).then(
      (r) => {
        if (r.ok) audio.current.delete(id);
        settle(id, r);
      },
      (e) => fail(id, e),
    );
  }, [nextId]);

  const dismiss = useCallback((id: string) => {
    audio.current.delete(id);
    setPending((p) => p.filter((x) => x.id !== id));
  }, []);
  const retry = useCallback(
    (p: PendingAsk) => {
      const pcm = audio.current.get(p.id);
      dismiss(p.id);
      if (p.source === "dashboard_mic" && pcm) sendAudio(pcm);
      else if (p.text) send(p.text);
    },
    [dismiss, send, sendAudio],
  );

  const requests = useMemo(() => [...(view?.Requests ?? [])].reverse(), [view]);
  const busy = pending.some((p) => !p.error);
  return (
    <ChatCtx.Provider value={{ view, requests, pending, loadError, busy, send, sendAudio, retry, dismiss }}>{children}</ChatCtx.Provider>
  );
}

// ---------- formatting ----------

export const fmtClock = (iso: string | Date) =>
  (typeof iso === "string" ? new Date(iso) : iso).toLocaleTimeString([], { hour: "numeric", minute: "2-digit" });

/** "840 ms" / "1.2 s" (the server's formatMS) back to milliseconds. */
export const parseMS = (s: string) => {
  const m = /^([\d.]+)\s*(ms|s)$/.exec(s.trim());
  if (!m) return 0;
  return m[2] === "s" ? parseFloat(m[1]) * 1000 : parseFloat(m[1]);
};
export const fmtMS = (ms: number) => (ms < 1000 ? `${Math.round(ms)} ms` : `${(ms / 1000).toFixed(1)} s`);
export const totalMS = (r: Request) => (r.Stages ?? []).reduce((n, s) => n + parseMS(s.MS), 0);

export const isFailed = (r: Request) => r.Outcome === "failed";
export const outcomeTone = (o: string): Tone => (o === "failed" ? "bad" : o === "cancelled" ? "warn" : "neutral");
export const isTyped = isTypedSource;

const SOURCE_ICON: Record<string, typeof Mic> = { voice: Mic, dashboard: Keyboard, dashboard_mic: MonitorSmartphone, debug: Keyboard };
export const sourceIcon = (s: string) => SOURCE_ICON[s] ?? Keyboard;

export function SourceTag({ source, label }: { source: string; label: string }) {
  const I = sourceIcon(source);
  return (
    <Badge>
      <I className="size-3" />
      {label}
    </Badge>
  );
}

/** Only outcomes worth a second look get a badge; "No audio" is normal for typed requests. */
export function OutcomeBadge({ r }: { r: Request }) {
  if (r.Outcome === "ok" || (r.Outcome === "no_audio" && !r.Error)) return null;
  return <Badge tone={outcomeTone(r.Outcome)}>{r.OutcomeText}</Badge>;
}

// ---------- request detail ----------

const STAGE_SHADE = [
  "bg-neutral-800 dark:bg-neutral-100",
  "bg-neutral-600 dark:bg-neutral-300",
  "bg-neutral-500 dark:bg-neutral-400",
  "bg-neutral-400 dark:bg-neutral-500",
  "bg-neutral-300 dark:bg-neutral-600",
  "bg-neutral-200 dark:bg-neutral-700",
];

function StageBar({ r }: { r: Request }) {
  const stages = r.Stages ?? [];
  const total = totalMS(r) || 1;
  return (
    <div className="space-y-2">
      <div className="flex h-2 w-full gap-px overflow-hidden rounded-full bg-default">
        {stages.map((s, i) => (
          <div key={s.Name} className={STAGE_SHADE[i % 6]} style={{ width: `${(parseMS(s.MS) / total) * 100}%` }} title={`${s.Label} ${s.MS}`} />
        ))}
      </div>
      <div className="grid grid-cols-1 gap-x-4 gap-y-1 text-xs min-[420px]:grid-cols-2 sm:grid-cols-3">
        {stages.map((s, i) => (
          <div key={s.Name} className="flex items-center gap-1.5">
            <span className={cn("size-2 shrink-0 rounded-sm", STAGE_SHADE[i % 6])} />
            <span className="text-muted">{s.Label}</span>
            <span className="ml-auto font-mono tabular-nums">{s.MS}</span>
          </div>
        ))}
      </div>
    </div>
  );
}

export function RequestDetail({ r, showReply }: { r: Request; showReply?: boolean }) {
  const rows: [string, ReactNode][] = [
    ["Path", r.PathLabel],
    ["Tool", r.Tool ? <span className="font-mono">{r.Tool}</span> : "—"],
    ["Source", r.SourceLabel],
    ["Outcome", r.OutcomeText],
    ["Response", r.Measured ? <span className="font-mono">{r.Response}</span> : <span className="text-muted">not measured</span>],
  ];
  for (const c of r.Checkpoints ?? []) rows.push([c.Label, <span className="font-mono">{c.MS}</span>]);
  return (
    <div className="space-y-3 text-sm">
      {showReply && r.Reply && <p className="leading-relaxed whitespace-pre-wrap">{r.Reply}</p>}
      {r.Error && <div className="border border-danger/30 bg-danger/8 px-3 py-2 font-mono text-xs break-words text-danger">{r.Error}</div>}
      <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1 text-xs">
        {rows.map(([k, v]) => (
          <div key={k} className="contents">
            <dt className="text-muted">{k}</dt>
            <dd className="min-w-0 break-words">{v}</dd>
          </div>
        ))}
      </dl>
      {(r.Stages?.length ?? 0) > 0 && <StageBar r={r} />}
      {(r.Calls?.length ?? 0) > 0 && (
        <div className="space-y-1.5">
          <div className="text-xs text-muted">Tool calls</div>
          {r.Calls!.map((c, i) => (
            <div key={i} className="border bg-surface px-3 py-2 text-xs">
              <div className="flex items-center gap-2">
                <span className="font-mono">{c.Name}</span>
                {c.LocalOnly && (
                  <Badge>
                    <Lock className="size-3" />
                    Local only
                  </Badge>
                )}
              </div>
              {c.Result && <pre className="mt-1.5 font-mono text-[11px] break-words whitespace-pre-wrap text-muted">{c.Result}</pre>}
            </div>
          ))}
        </div>
      )}
    </div>
  );
}

// ---------- one exchange in the conversation ----------

function UserBubble({ text, source, label, at }: { text: ReactNode; source: string; label: string; at: string | Date }) {
  return (
    <div className="flex flex-col items-end gap-1">
      <div className="max-w-[85%] border bg-surface-secondary px-3.5 py-2 text-sm break-words whitespace-pre-wrap">{text}</div>
      <div className="flex items-center gap-1.5 text-[11px] text-muted">
        <SourceTag source={source} label={label} />
        {fmtClock(at)}
      </div>
    </div>
  );
}

function BotRow({ children }: { children: ReactNode }) {
  return (
    <div className="flex gap-3">
      <span className="mt-0.5 flex size-7 shrink-0 items-center justify-center border">
        <Bot className="size-4" strokeWidth={1.5} />
      </span>
      <div className="min-w-0 max-w-[85%] flex-1 space-y-1">{children}</div>
    </div>
  );
}

export function Exchange({ r }: { r: Request }) {
  const ms = totalMS(r);
  return (
    <div className="space-y-3">
      <UserBubble text={r.Transcript} source={r.Source} label={r.SourceLabel} at={r.TimeISO} />
      <BotRow>
        <p className="py-1 text-[15px] leading-relaxed break-words whitespace-pre-wrap">
          {r.Reply || <span className={isFailed(r) ? "text-danger" : "text-muted"}>{r.Error || "No reply text."}</span>}
        </p>
        <Fold
          triggerClassName="flex w-auto items-center gap-1.5 py-0.5 text-[11px] text-subtle hover:text-foreground"
          bodyClassName="mt-1.5 border bg-surface-secondary p-3"
          trigger={
            <>
              <ChevronDown className={cn("size-3", foldOpen)} />
              {r.Tool ? <span className="font-mono">{r.Tool}</span> : <span>{r.PathLabel}</span>}
              {ms > 0 && <span>· {fmtMS(ms)}</span>}
              <OutcomeBadge r={r} />
            </>
          }
        >
          <RequestDetail r={r} />
        </Fold>
      </BotRow>
    </div>
  );
}

export function PendingExchange({ p }: { p: PendingAsk }) {
  const { retry, dismiss } = useChat();
  const mic = p.source === "dashboard_mic";
  return (
    <div className="space-y-3">
      <UserBubble
        text={p.text || <span className="text-muted italic">{mic ? (p.error ? "Recording" : "Transcribing…") : ""}</span>}
        source={p.source}
        label={mic ? "Dashboard mic" : "Dashboard typed"}
        at={p.at}
      />
      <BotRow>
        {p.error ? (
          <div className="space-y-2 py-1">
            <p className="text-sm text-danger">{p.error}</p>
            <div className="flex gap-1">
              <Button size="sm" variant="secondary" onPress={() => retry(p)}>
                <RotateCcw className="size-3.5" /> Retry
              </Button>
              <Button size="sm" variant="ghost" onPress={() => dismiss(p.id)}>
                Dismiss
              </Button>
            </div>
          </div>
        ) : (
          <div className="flex gap-1 py-2.5" aria-label="Thinking">
            {[0, 1, 2].map((i) => (
              <span key={i} className="size-1.5 animate-bounce rounded-full bg-muted" style={{ animationDelay: `${i * 120}ms` }} />
            ))}
          </div>
        )}
      </BotRow>
    </div>
  );
}

// ---------- composer ----------

/**
 * Prompt panel: text on top, toolbar below (caller's slot, mic, send).
 * Enter sends, Shift+Enter is a new line. The mic records until Stop (or the
 * 30 s cap), then uploads the utterance for transcription.
 */
export function Composer({ placeholder = "Ask clanka…", onSent, toolbar, className }: { placeholder?: string; onSent?: () => void; toolbar?: ReactNode; className?: string }) {
  const { send, sendAudio } = useChat();
  const session = useSession();
  const [text, setText] = useState("");
  const { phase, elapsed, levels, error: micError, start, stop, cancel } = useRecorder({ onSend: sendAudio, onSent });
  const canMic = session.Mic && micSupported();
  const recording = phase === "recording";

  const submit = () => {
    const t = text.trim();
    if (!t) return;
    send(t);
    setText("");
    onSent?.();
  };

  if (!session.Typed) {
    return <div className={cn("border bg-surface-secondary px-4 py-3 text-sm text-muted", className)}>Typed requests are not wired in this deployment.</div>;
  }

  const micButton = (
    <Button
      isIconOnly
      variant="ghost"
      size="sm"
      onPress={start}
      isDisabled={!canMic || phase === "starting"}
      aria-label={canMic ? "Talk" : session.Mic ? "The microphone needs HTTPS" : "Speech recognition is not configured"}
    >
      <AudioLines className="size-4" />
    </Button>
  );

  return (
    <div className={cn("border bg-surface-secondary", className)}>
      {recording ? (
        <div className="flex min-h-[6.25rem] items-center gap-2 px-2">
          <Button isIconOnly variant="ghost" onPress={cancel} aria-label="Cancel recording">
            <X className="size-5" />
          </Button>
          {/* Red is reserved for "recording". */}
          <span className="relative flex size-2.5 shrink-0">
            <span className="absolute inset-0 animate-ping rounded-full bg-danger opacity-60" />
            <span className="relative size-2.5 rounded-full bg-danger" />
          </span>
          <div className="flex h-8 min-w-0 flex-1 items-center gap-[3px] overflow-hidden" aria-hidden>
            {levels.map((l, i) => (
              <span key={i} className="w-1 flex-1 rounded-full bg-foreground/70 transition-[height] duration-75" style={{ height: `${l * 100}%` }} />
            ))}
          </div>
          <span className="w-12 text-right font-mono text-xs tabular-nums text-muted">
            {elapsed}s / {MIC_MAX_SECONDS}s
          </span>
          <Button isIconOnly variant="danger" onPress={stop} aria-label="Stop and send">
            <Square className="size-4 fill-current" />
          </Button>
        </div>
      ) : (
        <form
          className="flex flex-col"
          onSubmit={(e) => {
            e.preventDefault();
            submit();
          }}
        >
          <textarea
            rows={1}
            value={text}
            maxLength={2000}
            onChange={(e) => setText(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter" && !e.shiftKey && !e.nativeEvent.isComposing) {
                e.preventDefault();
                submit();
              }
            }}
            placeholder={placeholder}
            aria-label="Message"
            className="field-sizing-content max-h-40 min-h-12 w-full flex-1 resize-none bg-transparent px-4 pt-3.5 text-sm outline-none placeholder:text-muted"
          />
          <div className="flex items-center gap-1 px-2 pt-1 pb-2">
            <div className="flex min-w-0 flex-1 items-center gap-1.5 text-muted">
              <SpeakHint />
              {toolbar}
              {micError && <span className="truncate pl-1 text-xs text-danger">{micError}</span>}
            </div>
            {canMic ? (
              micButton
            ) : (
              <Tooltip delay={0}>
                <Tooltip.Trigger>
                  <span tabIndex={0}>{micButton}</span>
                </Tooltip.Trigger>
                <Tooltip.Content>{session.Mic ? "The microphone needs HTTPS (open the dashboard through tailscale serve)." : "Speech recognition is not configured."}</Tooltip.Content>
              </Tooltip>
            )}
            <Button isIconOnly type="submit" size="sm" isDisabled={!text.trim()} aria-label="Send">
              <ArrowUp className="size-4" />
            </Button>
          </div>
        </form>
      )}
    </div>
  );
}
