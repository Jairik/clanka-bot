import { useState } from "react";
import { Button } from "@heroui/react";
import { postForm } from "./api";
import type { RoutingExplainView } from "./types";
import { Badge } from "./ui";

// ExplainRoutingPanel renders the read-only explain routing panel on the Tools settings page.
export function ExplainRoutingPanel() {
  const [phrase, setPhrase] = useState("");
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [result, setResult] = useState<RoutingExplainView | null>(null);

  const handleExplain = async (e?: React.FormEvent) => {
    e?.preventDefault();
    const text = phrase.trim();
    if (!text) return;
    setLoading(true);
    setError(null);
    try {
      const res = await postForm<RoutingExplainView>("/api/ui/routing/explain", { text });
      if (res.ok && res.view) {
        setResult(res.view);
      } else {
        setError(res.text || "Failed to explain routing.");
      }
    } catch (err) {
      setError(err instanceof Error ? err.message : "Request failed.");
    } finally {
      setLoading(false);
    }
  };

  return (
    <div className="border-b border-separator px-3.5 py-3 space-y-3 bg-surface-secondary/20">
      <div>
        <div className="text-sm font-semibold">Explain routing</div>
        <p className="text-xs text-muted mt-0.5">
          Type a phrase to inspect routing decisions and similarity scores. Explanations are side-effect free.
        </p>
      </div>

      <form onSubmit={handleExplain} className="flex gap-2">
        <input
          type="text"
          value={phrase}
          onChange={(e) => setPhrase(e.target.value)}
          placeholder="e.g. put the kitchen lights on warm"
          aria-label="Phrase to explain"
          className="flex-1 rounded-lg border border-separator bg-surface px-3 py-1.5 text-xs text-foreground placeholder:text-muted focus:outline-none focus:ring-1 focus:ring-accent"
        />
        <Button
          type="submit"
          size="sm"
          isDisabled={!phrase.trim() || loading}
          isPending={loading}
          className="shrink-0 text-xs"
        >
          Explain
        </Button>
      </form>

      {error && <p className="text-xs text-danger">{error}</p>}

      {result && (
        <div className="rounded-lg border border-separator bg-surface p-3 space-y-2.5 text-xs">
          <div className="flex flex-wrap items-center gap-2">
            <span className="font-semibold text-muted">Decision:</span>
            <span className="font-mono text-foreground font-medium" data-testid="decision-line">
              {result.decision || result.path}
            </span>
            <Badge tone="neutral">{result.path}</Badge>
            {result.tool && <Badge tone="ok">{result.tool}</Badge>}
            {result.chain_split && <Badge tone="warn">chain split</Badge>}
            {result.focus_owner && <Badge tone="neutral">focus: {result.focus_owner}</Badge>}
          </div>

          {result.fallthrough_reason && (
            <div className="flex items-center gap-2 text-xs">
              <span className="text-muted">Reason:</span>
              <span className="font-mono text-muted">{result.fallthrough_reason}</span>
            </div>
          )}

          {result.args && (
            <div className="flex items-start gap-2 text-xs">
              <span className="text-muted shrink-0">Arguments:</span>
              <pre className="font-mono text-[11px] bg-surface-secondary/40 p-1.5 rounded border border-separator overflow-x-auto max-w-full">
                {typeof result.args === "string" ? result.args : JSON.stringify(result.args, null, 2)}
              </pre>
            </div>
          )}

          {result.top && result.top.length > 0 && (
            <div className="space-y-1.5">
              <div className="flex items-center justify-between text-muted text-[11px]">
                <span>Top Tools</span>
                {result.margin > 0 && <span>Margin: {result.margin.toFixed(3)}</span>}
              </div>
              <div className="rounded border border-separator overflow-hidden">
                <table className="w-full text-left text-xs" aria-label="Top tools and scores">
                  <thead className="border-b border-separator bg-surface-secondary/50 text-[11px] text-muted">
                    <tr>
                      <th className="px-2.5 py-1.5 font-medium">Tool</th>
                      <th className="px-2.5 py-1.5 font-medium text-right">Score</th>
                    </tr>
                  </thead>
                  <tbody className="divide-y divide-separator font-mono text-[11px]">
                    {result.top.map((score, i) => (
                      <tr key={score.tool || i} className="hover:bg-surface-secondary/30">
                        <td className="px-2.5 py-1">{score.tool}</td>
                        <td className="px-2.5 py-1 text-right">{score.score.toFixed(3)}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            </div>
          )}
        </div>
      )}
    </div>
  );
}
