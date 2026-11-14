// Hermes: what the MCP adapter would expose and which accounts it may use.
// Hermes itself is not part of this build, so the page says that plainly and
// treats every grant as stored intent.
import { useState } from "react";
import { Button, Switch } from "@heroui/react";
import { ChevronDown } from "lucide-react";
import { usePageData } from "../api";
import { Field, TextField, useActions, type Actions } from "../forms";
import { PageFrame } from "../shell";
import type { HermesView, Integration, IntegrationSecret } from "../types";
import { Badge, Dot, Empty, Fold, Lede, Notice, Section, cn, foldOpen } from "../ui";

export function HermesPage() {
  const { data, error, set } = usePageData("hermes");
  const actions = useActions<HermesView>(set);
  const rows = data?.Rows ?? [];
  const linked = rows.filter((r) => r.StatusKey === "linked" || r.StatusKey === "environment").length;

  return (
    <PageFrame id="hermes">
      <Notice error={error || actions.error} flash={actions.flash} onDismiss={actions.clear} />
      <div className="space-y-2.5">
        <Section title="Hermes is not installed" summary="not running" tone="warn" defaultOpen flush>
          <div className="space-y-2 px-3.5 py-3 text-xs leading-relaxed">
            <p>
              Hermes is not installed in this build, so no process, job queue or MCP adapter is running and nothing on this page starts
              one.
            </p>
            <p className="text-muted">
              This page sets up what Hermes would get once the adapter is installed: the Tools it could call and the accounts it could
              use. Everything here is stored intent until then.
            </p>
          </div>
        </Section>

        {data && !data.HasRegistry && (
          <Section title="The Tool registry is not configured" defaultOpen>
            <Empty>This Dashboard was built without the Tool registry, so there is no surface to show.</Empty>
          </Section>
        )}

        {data?.HasRegistry && (
          <Section title="What Hermes would see" summary={`${data.SurfaceCount} tools`} defaultOpen flush>
            <Lede>
              Every Tool that may leave the Pi, is not Pi-only and does not need a spoken Confirmation. Hermes would call these over MCP.
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
              <Empty>No Tool qualifies, so Hermes would get an empty surface.</Empty>
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
  const ready = row.StatusKey === "linked" || row.StatusKey === "environment";
  const missing = secrets.some((s) => !s.Optional && !(drafts[s.Name] ?? "").trim());
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
            <span className="text-[11px] text-subtle">
              {row.Linkable
                ? missing
                  ? "Fill every required field to save."
                  : "Saving writes only the values you type."
                : "The secrets service is not available in this build."}
            </span>
          </div>
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
                    ? "Stored intent: it applies once Hermes is installed."
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

function secretHint(s: IntegrationSecret) {
  if (!s.Set) return "Not set";
  const source = s.Source === "env" ? "from .env" : s.Source === "store" ? "stored" : "set";
  return s.Last4 ? `Set — ${source}, ending ${s.Last4}` : `Set — ${source}, last 4 hidden`;
}
