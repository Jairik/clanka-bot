import { Button } from "@heroui/react";
import { RefreshCw } from "lucide-react";
import { usePageData } from "../api";
import { PageFrame } from "../shell";
import type { Check, StatusState } from "../types";
import { Dot, Empty, Notice, Section, cn, type Tone } from "../ui";

const TONE: Record<StatusState, Tone> = { ok: "ok", error: "bad", "timed-out": "bad", "not-configured": "neutral", "no-check": "neutral" };
const failing = (c: Check) => TONE[c.Status] === "bad";

export function HealthPage() {
  // A minute is frequent enough for a status page without hammering the checks.
  const { data, error, loading, reload } = usePageData("health", { poll: 60_000 });
  const groups: [string, Check[], string?][] = data
    ? [
        ["Compose services", data.Services ?? []],
        ["Wake word", data.Wakeword ?? [], "The wake and wake-free command models the Assistant listens for, with the detection threshold it applies to each one."],
        ["Location", data.Location ? [data.Location] : []],
        ["Tool modules", data.Modules ?? []],
        ["Spotify", data.Spotify ?? [], "Connect pairing and Web API auth are separate grants. One can be healthy while the other is not."],
      ]
    : [];

  return (
    <PageFrame
      id="health"
      right={
        <div className="flex items-center gap-3 text-xs text-muted">
          {data && <span>Checked {data.CheckedAt}</span>}
          <Button size="sm" variant="secondary" onPress={() => void reload()} isPending={loading}>
            <RefreshCw className="size-3.5" /> Refresh
          </Button>
        </div>
      }
    >
      <Notice error={error} />
      {!data && !error && <Empty>Checking every service… this can take up to the health timeout.</Empty>}
      <div className="space-y-2.5">
        {groups.map(([title, checks, note]) => {
          const bad = checks.filter(failing).length;
          const ok = checks.filter((c) => c.Status === "ok").length;
          return (
            <Section
              key={title}
              title={title}
              tone={bad ? "bad" : ok ? "ok" : "neutral"}
              defaultOpen={bad > 0 || title === "Compose services"}
              flush
              summary={checks.length ? (bad ? `${bad} failing` : `${ok}/${checks.length} ok`) : "none"}
            >
              {note && <p className="px-3.5 pt-3 text-xs text-muted">{note}</p>}
              {checks.length ? (
                <ul className="divide-y divide-separator">
                  {checks.map((c) => (
                    <li key={c.Name} className="flex min-h-12 items-start gap-3 px-3.5 py-3">
                      <span className="mt-1.5">
                        <Dot tone={TONE[c.Status] ?? "neutral"} pulse={failing(c)} />
                      </span>
                      <div className="min-w-0 flex-1">
                        <div className="flex flex-wrap items-baseline gap-x-2">
                          <span className="font-mono text-sm">{c.Name}</span>
                          <span className={cn("text-xs", failing(c) ? "text-danger" : "text-muted")}>{c.StatusLabel}</span>
                        </div>
                        {c.Detail && <p className="mt-0.5 text-xs break-words text-muted">{c.Detail}</p>}
                      </div>
                    </li>
                  ))}
                </ul>
              ) : (
                <Empty>No checks are wired.</Empty>
              )}
            </Section>
          );
        })}
      </div>
      {data && <p className="mt-4 text-xs text-subtle">One refresh is bounded to {data.HealthTimeout}. The page refreshes every minute.</p>}
    </PageFrame>
  );
}
