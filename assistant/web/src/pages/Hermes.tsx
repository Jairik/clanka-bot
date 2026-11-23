// Hermes offers model-only jobs. Account grants are enforced on the allowlist.
import { useState } from "react";
import { Button, Switch } from "@heroui/react";
import { ChevronDown } from "lucide-react";
import { usePageData } from "../api";
import { Field, TextField, useActions, type Actions } from "../forms";
import { PageFrame } from "../shell";
import { HermesToolsSection } from "./HermesTools";
import type { HermesView, Integration, IntegrationSecret } from "../types";
import { Badge, Dot, Empty, Fold, Lede, Notice, Section, cn, foldOpen, type Tone } from "../ui";

export function HermesPage() {
  const { data, error, set } = usePageData("hermes");
  const actions = useActions<HermesView>(set);
  const rows = data?.Rows ?? [];
  const linked = rows.filter((r) => r.StatusKey === "linked" || r.StatusKey === "environment").length;

  return (
    <PageFrame id="hermes">
      <Notice error={error || actions.error} flash={actions.flash} onDismiss={actions.clear} />
      <div className="space-y-2.5">
        <Section
          title={data?.RuntimeReady ? "Hermes model-only jobs" : "Hermes needs setup"}
          summary={data?.RuntimeReady ? "configured" : "unconfigured"}
          tone={data?.RuntimeReady ? undefined : "warn"}
          defaultOpen
          flush
        >
          <div className="space-y-2 px-3.5 py-3 text-xs leading-relaxed">
            {data?.RuntimeReady ? (
              <p>Start a job with “Ask Hermes” followed by a request. Say “Ask Hermes status”, “Ask Hermes result”, or “Ask Hermes cancel” to manage the latest job.</p>
            ) : (
              <p>Enable the private sidecar on the Pi and add its OpenCode Go key in Secrets.</p>
            )}
            <p className="text-muted">
              {data?.AdapterReady
                ? "The private adapter enforces the allowlist and live account grants on every call. A job lists Tool names only and loads a schema when it asks to use one."
                : "The model-only runtime uses the current request. The Tool adapter is not wired in this process."}
            </p>
          </div>
        </Section>

        {data && !data.HasRegistry && (
          <Section title="The Tool registry is not configured" defaultOpen>
            <Empty>This Dashboard was built without the Tool registry, so there is no surface to show.</Empty>
          </Section>
        )}

        {data?.HasRegistry && (
          <Section title="What Hermes can call" summary={`${data.SurfaceCount} tools`} defaultOpen flush>
            <Lede>
              {data.AdapterReady
                ? "The enforced allowlist after live grants. Names are listed without schemas; a selected Tool loads its schema when requested."
                : "The adapter is not installed, so the enforced surface is empty."}
            </Lede>
            {data.Surface?.length ? (
              <ul className="divide-y divide-separator">
                {data.Surface.map((tool) => (
                  <li key={tool.Name} className="flex min-h-10 items-center gap-3 px-3.5 py-1.5">
                    <span className="min-w-0 flex-1 truncate font-mono text-xs">{tool.Name}</span>
                    <Badge>{tool.Permission}</Badge>
                  </li>
                ))}
              </ul>
            ) : (
              <Empty>No Tool is currently allowed. Grant an allowlisted account, or wait until a Tool is on the allowlist.</Empty>
            )}
            {!!data.WithheldCount && (
              <div className="border-t px-3.5 py-2.5 text-[11px] text-muted">
                {data.WithheldCount} allowlisted {data.WithheldCount === 1 ? "Tool is" : "Tools are"} withheld until the matching account grant is on
                {data.Withheld?.length ? `: ${data.Withheld.map((tool) => tool.Name).join(", ")}` : "."}
              </div>
            )}
            <dl className="grid gap-px border-t bg-separator sm:grid-cols-2">
              <div className="bg-surface px-3.5 py-2.5">
                <dt className="flex items-center gap-1.5 text-xs">
                  Pi only <Badge>never leaves</Badge>
                </dt>
                <dd className="mt-0.5 text-[11px] text-muted">
                  {data.LocalOnlyCount} Tools run on the Pi, so their requests and data cannot be handed to Hermes.
                </dd>
              </div>
              <div className="bg-surface px-3.5 py-2.5">
                <dt className="flex items-center gap-1.5 text-xs">
                  Confirm <Badge>asks first</Badge>
                </dt>
                <dd className="mt-0.5 text-[11px] text-muted">
                  {data.ConfirmCount} Tools need a spoken confirmation, so they are withheld from the surface.
                </dd>
              </div>
            </dl>
          </Section>
        )}

        {data && <HermesToolsSection />}

        {data?.HasIntegrations ? (
          <Section title="Accounts" summary={`${linked} linked · ${data.Granted} allowed`} defaultOpen flush>
            <Lede>
              Values are write-only: enter one to store it, and nothing on this page can read one back. Linking writes the same secrets
              the Tools already read.
            </Lede>
            {rows.length ? (
              <ul className="divide-y divide-separator">
                {rows.map((row) => (
                  <li key={row.ID}>
                    <Fold
                      triggerClassName="flex min-h-14 items-center gap-3 px-3.5 py-2.5 hover:bg-surface-secondary"
                      bodyClassName="border-t border-dashed bg-surface-secondary px-3.5 py-3"
                      trigger={
                        <>
                          <Dot tone={statusTone(row.StatusKey)} />
                          <div className="min-w-0 flex-1">
                            <div className="flex flex-wrap items-center gap-1.5">
                              <span className="truncate text-sm">{row.Name}</span>
                              <Badge tone={statusTone(row.StatusKey)}>{row.Status}</Badge>
                              {row.Hermes && <Badge className="bg-foreground text-background">Hermes allowed</Badge>}
                            </div>
                            <div className="mt-0.5 truncate text-[11px] text-muted">{row.Summary}</div>
                          </div>
                          <ChevronDown className={cn("size-4 shrink-0 text-muted", foldOpen)} />
                        </>
                      }
                    >
                      <IntegrationEditor row={row} actions={actions} />
                    </Fold>
                  </li>
                ))}
              </ul>
            ) : (
              <Empty>No account is declared.</Empty>
            )}
          </Section>
        ) : (
          data && (
            <Section title="Accounts are not configured" defaultOpen>
              <Empty>This Dashboard was built without the integrations or secrets service, so no account can be linked.</Empty>
            </Section>
          )
        )}
      </div>
    </PageFrame>
  );
}

/** One account: what it unlocks, its write-only secrets, and the Hermes grant. */
function IntegrationEditor({ row, actions }: { row: Integration; actions: Actions<HermesView> }) {
  const [drafts, setDrafts] = useState<Record<string, string>>({});
  const secrets = row.Secrets ?? [];
  const base = `/hermes/integrations/${encodeURIComponent(row.ID)}`;
  const linkAction = base;
  const unlinkAction = `${base}/unlink`;
  const grantAction = `${base}/grant`;
  const verifyAction = `${base}/verify`;
  const ready = row.StatusKey === "linked" || row.StatusKey === "environment";
  // Blank is fine when the account already stores a value; only a truly missing
  // one blocks. The server skips a blank value when the account already has one
  // (integrations.go), so an owner can re-save after a partial failure or set
  // only an optional secret without retyping the required, stored one.
  const missing = secrets.some((s) => !s.Optional && !s.Set && !(drafts[s.Name] ?? "").trim());
  const anythingSet = secrets.some((s) => s.Set);

  const link = async () => {
    const body: Record<string, string> = {};
    for (const s of secrets) body[s.Name] = drafts[s.Name] ?? "";
    if (await actions.run(linkAction, body)) setDrafts({});
  };

  return (
    <div className="space-y-3">
      {row.Docs && <p className="text-xs text-muted">{row.Docs}</p>}

      <div className="space-y-1">
        <div className="text-xs text-muted">Unlocks</div>
        {row.Tools?.length ? (
          <div className="flex flex-wrap gap-1">
            {row.Tools.map((tool) => (
              <Badge key={tool} className="font-mono">
                {tool}
              </Badge>
            ))}
          </div>
        ) : (
          <p className="text-[11px] text-subtle">Dashboard only: no Hermes Tools, so there is nothing to grant.</p>
        )}
      </div>

      {secrets.length > 0 && (
        <form
          className="space-y-3"
          autoComplete="off"
          onSubmit={(e) => {
            e.preventDefault();
            void link();
          }}
        >
          {secrets.map((s) => (
            <Field
              key={s.Name}
              label={`${s.Name}${s.Optional ? " (optional)" : ""}`}
              hint={secretHint(s)}
            >
              <TextField
                label={`${s.Name}${s.Optional ? " (optional)" : ""}`}
                type="password"
                mono
                autoComplete="new-password"
                value={drafts[s.Name] ?? ""}
                onChange={(v) => setDrafts((d) => ({ ...d, [s.Name]: v }))}
                placeholder={s.Set ? "Type a new value to replace it" : s.Optional ? "Optional" : "Required"}
              />
            </Field>
          ))}
          <div className="flex flex-wrap items-center gap-2">
            <Button
              size="sm"
              type="submit"
              isDisabled={!row.Linkable || missing}
              isPending={actions.pending === linkAction}
            >
              {ready ? "Replace values" : "Link account"}
            </Button>
            {anythingSet && (
              <Button
                size="sm"
                variant="danger-soft"
                isPending={actions.pending === unlinkAction}
                onPress={() => void actions.run(unlinkAction)}
              >
                Unlink
              </Button>
            )}
            {row.Linkable && (
              <Button
                size="sm"
                type="button"
                variant="ghost"
                isPending={actions.pending === verifyAction}
                isDisabled={actions.pending !== "" && actions.pending !== verifyAction}
                onPress={() => void actions.run(verifyAction)}
                aria-label={`Verify the stored key for ${row.Name}`}
              >
                Verify
              </Button>
            )}
            <span className="text-[11px] text-subtle">
              {row.Linkable
                ? missing
                  ? "Fill every required field to save."
                  : "Saving writes only the values you type."
                : "The secrets service is not available in this build."}
            </span>
          </div>
          {row.verify && (
            <div className="mt-2 flex flex-wrap items-center gap-2">
              <Badge tone={verifyTone(row.verify.class)}>{verifyLabel(row.verify.class)}</Badge>
              <span className="text-xs text-muted">{row.verify.reason}</span>
            </div>
          )}
        </form>
      )}

      {row.CanGrant ? (
        <div className="border-t pt-3">
          <Switch
            isSelected={row.Hermes}
            isDisabled={!ready || actions.pending === grantAction}
            onChange={(on) => void actions.run(grantAction, { allowed: on ? "on" : "" })}
            aria-label={`Allow Hermes to use ${row.Name}`}
          >
            <Switch.Content className="flex w-full items-center justify-between gap-3">
              <span className="min-w-0 text-left">
                <span className="block text-sm">Allow Hermes</span>
                <span className="mt-0.5 block text-[11px] text-muted">
                  {ready
                    ? "Enforced on every adapter call for Tools on the current allowlist."
                    : "Link the account before granting access."}
                </span>
              </span>
              <Switch.Control>
                <Switch.Thumb />
              </Switch.Control>
            </Switch.Content>
          </Switch>
        </div>
      ) : (
        <p className="border-t pt-3 text-[11px] text-subtle">
          This account has no Hermes surface, so there is no access to grant.
        </p>
      )}
    </div>
  );
}

const statusTone = (key: string): "ok" | "warn" | "neutral" =>
  key === "linked" || key === "environment" ? "ok" : key === "partial" ? "warn" : "neutral";

/** Verify result color. "ok" is the only good outcome; anything that shows the
 * key is wrong or the service is down is a warning, and unknown classes fall back
 * to neutral so the page never invents a status. */
const verifyTone = (cls: string): Tone =>
  cls === "ok" ? "ok" : cls === "rejected" || cls === "unreachable" ? "warn" : "neutral";

/** Human label for a verify class. Falls back to the raw class so an unknown
 * value is shown rather than silently dropped. */
const verifyLabel = (cls: string): string => {
  switch (cls) {
    case "ok":
      return "Works";
    case "rejected":
      return "Rejected";
    case "unreachable":
      return "Unreachable";
    case "not-configured":
      return "Not configured";
    default:
      return cls;
  }
};

function secretHint(s: IntegrationSecret) {
  if (!s.Set) return "Not set";
  const source = s.Source === "env" ? "from .env" : s.Source === "store" ? "stored" : "set";
  return s.Last4 ? `Set — ${source}, ending ${s.Last4}` : `Set — ${source}, last 4 hidden`;
}
