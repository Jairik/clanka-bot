import { Button, Checkbox } from "@heroui/react";
import { Download, RefreshCw } from "lucide-react";
import { Fragment, useState } from "react";
import { downloadDiagnostics, usePageData } from "../api";
import { PageFrame } from "../shell";
import type { Check } from "../types";
import { Dot, Empty, Notice, Section, cn, type Tone } from "../ui";

const TONE: Record<string, Tone> = { ok: "ok", error: "bad", "timed-out": "bad", "not-configured": "neutral", "no-check": "neutral" };
const failing = (c: Check) => TONE[c.Status] === "bad";

export function HealthPage() {
  // A minute is frequent enough for a status page without hammering the checks.
  const { data, error, refreshing, reload } = usePageData("health", { poll: 60_000 });
  const [includeRequests, setIncludeRequests] = useState(false);
  const [downloading, setDownloading] = useState(false);
  const [downloadError, setDownloadError] = useState("");
  const groups: [string, Check[], string?][] = data
    ? [
        ["Compose services", data.Services ?? []],
        ["Semantic routing", data.Routing ? [data.Routing] : [], "The semantic Fast path is optional: a degraded index leaves Control phrases and the Agent path working. This is the in-memory build state, not a service probe."],
        ["Wake word", data.Wakeword ?? [], "The wake and wake-free command models the Assistant listens for, with the detection threshold it applies to each one."],
        ["Location", data.Location ? [data.Location] : []],
        ["Tool modules", data.Modules ?? []],
        ["Spotify", data.Spotify ?? [], "Connect pairing and Web API auth are separate grants. One can be healthy while the other is not."],
      ]
    : [];

  const download = async () => {
    if (!data) return;
    setDownloading(true);
    setDownloadError("");
    try {
      await downloadDiagnostics(data.DiagnosticsURL, includeRequests);
    } catch (e) {
      setDownloadError(e instanceof Error ? e.message : String(e));
    } finally {
      setDownloading(false);
    }
  };

  return (
    <PageFrame
      id="health"
      right={
        <div className="flex items-center gap-3 text-xs text-muted">
          {data && (
            <span aria-live="polite">
              {refreshing ? "Checking…" : `Checked ${data.CheckedAt}`}
            </span>
          )}
          <Button size="sm" variant="secondary" onPress={() => void download()} isPending={downloading} isDisabled={!data}>
            <Download className="size-3.5" /> Download diagnostics
          </Button>
          <Button
            size="sm"
            variant="secondary"
            onPress={() => void reload()}
            isPending={refreshing}
            isDisabled={!data || refreshing}
          >
            <RefreshCw className="size-3.5" /> Refresh
          </Button>
        </div>
      }
    >
      <Notice error={downloadError || error} />
      <p className="mb-2.5 text-xs text-subtle">
        The diagnostics file is for support: it contains the health snapshot, effective settings with secrets redacted, versions and Tool policy. Nothing is uploaded.
      </p>
      <Checkbox isSelected={includeRequests} onChange={setIncludeRequests} className="mb-3">
        <Checkbox.Content className="text-xs text-muted">
          <Checkbox.Control>
            <Checkbox.Indicator />
          </Checkbox.Control>
          Include the last 50 requests in the file
        </Checkbox.Content>
      </Checkbox>
      <p className="mb-3 text-xs text-subtle">Including requests adds private transcript and reply text to the file. Review it before sharing.</p>
      {!data && !error && <Empty>Checking every service… this can take up to the health timeout.</Empty>}
      <div className="space-y-2.5">
        {groups.map(([title, checks, note]) => {
          const bad = checks.filter(failing).length;
          const ok = checks.filter((c) => c.Status === "ok").length;
          const unchecked = checks.filter((c) => c.Status === "no-check").length;
          const unconfigured = checks.filter((c) => c.Status === "not-configured").length;
          // Modules that declare no health check are neither ok nor failing.
          // Counting them in the denominator read "4/19 ok" next to the home
          // card's "12/12"; naming them is honest and matches that card, which
          // counts only real checks.
          const parts = [
            bad ? `${bad} failing` : ok ? `${ok} ok` : "",
            unchecked ? `${unchecked} no check` : "",
            unconfigured ? `${unconfigured} not configured` : "",
          ].filter(Boolean);
          const summary = checks.length ? parts.join(" · ") : "none";
          return (
            <Fragment key={title}>
              <Section
                title={title}
                tone={bad ? "bad" : ok ? "ok" : "neutral"}
                defaultOpen={bad > 0 || title === "Compose services"}
                flush
                summary={summary}
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
              {title === "Wake word" && data?.FalseWakes?.length ? (
                <Section
                  title="False wakes (since restart)"
                  tone="neutral"
                  defaultOpen
                  flush
                  summary={`${data.FalseWakes.length} model${data.FalseWakes.length === 1 ? "" : "s"}`}
                >
                  <p className="px-3.5 pt-3 text-xs text-muted">
                    Wakes that produced no speech or nothing routable, counted in memory since the Assistant restarted.
                  </p>
                  <ul className="divide-y divide-separator">
                    {data.FalseWakes.map((fw) => (
                      <li key={fw.Model} className="flex min-h-12 items-start gap-3 px-3.5 py-3">
                        <div className="min-w-0 flex-1">
                          <div className="flex flex-wrap items-baseline gap-x-2">
                            <span className="font-mono text-sm">{fw.Model}</span>
                            <span className="text-xs text-muted">false-wake rate {fw.Rate}</span>
                          </div>
                          <p className="mt-0.5 text-xs break-words text-muted">
                            {fw.NoSpeech} no speech · {fw.Unroutable} unroutable · {fw.Wakes} wakes
                          </p>
                        </div>
                      </li>
                    ))}
                  </ul>
                </Section>
              ) : null}
            </Fragment>
          );
        })}
      </div>
      {data && <p className="mt-4 text-xs text-subtle">One refresh is bounded to {data.HealthTimeout}. The page refreshes every minute.</p>}
    </PageFrame>
  );
}
