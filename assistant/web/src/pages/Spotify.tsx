// Spotify: Connect pairing (librespot) and Web API auth are separate grants.
// Connect/Verify/Disconnect are real form posts: Connect has to leave for
// accounts.spotify.com, and all three come back as /spotify?flash=...
import { useEffect } from "react";
import { Button } from "@heroui/react";
import { navigate, useLocation, usePageData } from "../api";
import { PageFrame } from "../shell";
import { Dot, Empty, Lede, Meta, Notice, Section, SourceBadge, type Tone } from "../ui";

// A pill is a plain string on the wire; anything but ok/error is neutral.
const tone = (pill: string): Tone => (pill === "ok" ? "ok" : pill === "error" ? "bad" : "neutral");

function PostButton({ action, children, variant = "secondary" }: { action: string; children: string; variant?: "primary" | "secondary" | "danger-soft" }) {
  return (
    <form method="post" action={action}>
      <Button size="sm" type="submit" variant={variant}>
        {children}
      </Button>
    </form>
  );
}

export function SpotifyPage() {
  const { params } = useLocation();
  const flashKey = params.get("flash") ?? "";
  const { data, error } = usePageData("spotify", { query: flashKey ? `flash=${encodeURIComponent(flashKey)}` : undefined });
  // Drop ?flash= once read so a reload doesn't repeat the message.
  useEffect(() => {
    if (flashKey && data) navigate("/spotify", { replace: true });
  }, [flashKey, data]);

  const p = data?.Pairing;
  const w = data?.WebAPI;
  return (
    <PageFrame id="spotify">
      <Notice error={error || data?.Error} flash={data?.Flash} />
      {p && w && (
        <div className="space-y-2.5">
          <Section title="Spotify Connect" summary={p.Label} tone={tone(p.Pill)} defaultOpen flush>
            <div className="space-y-3 px-3.5 py-3">
              <div className="flex items-start gap-2.5">
                <span className="mt-1.5">
                  <Dot tone={tone(p.Pill)} />
                </span>
                <p className="text-sm">{p.Detail}</p>
              </div>
              <Meta items={[["Device", p.Configured && p.Device && <span className="font-mono">{p.Device}</span>], ["Last heartbeat", p.Updated && <time dateTime={p.UpdatedISO}>{p.Updated}</time>]]} />
              {!p.Configured && <p className="text-xs text-muted">The librespot status volume is not mounted, so pairing cannot be reported. The container may still be running.</p>}
              {p.Configured && !p.Available && (
                <p className="text-xs text-muted">
                  The heartbeat is missing or stale. Check <code>docker compose ps librespot</code> and <code>docker compose logs librespot</code>.
                </p>
              )}
              <ol className="list-decimal space-y-1 pl-5 text-xs text-muted">
                <li>Put the phone or desktop on the same LAN as the Pi.</li>
                <li>
                  Play something in Spotify, open <span className="text-foreground">Devices</span>, and choose <code className="text-foreground">{p.Device || "Clanka"}</code>.
                </li>
                <li>The first connection caches credentials on librespot's own volume; later starts reuse them.</li>
              </ol>
            </div>
          </Section>

          <Section title="Web API" summary={w.Label} tone={tone(w.Pill)} defaultOpen flush>
            <Lede>Authorization Code with PKCE. The refresh token is the only Spotify credential stored; access tokens stay in memory.</Lede>
            <div className="space-y-3 px-3.5 py-3">
              <div className="flex items-start gap-2.5">
                <span className="mt-1.5">
                  <Dot tone={tone(w.Pill)} />
                </span>
                <p className="text-sm">{w.Detail}</p>
              </div>
              <Meta
                items={[
                  ["Token", <span className="inline-flex items-center gap-1.5">{w.Set ? "set" : "not set"} <SourceBadge source={w.Source} label={w.SourceLabel} /></span>],
                  ["Updated", w.Updated && <time dateTime={w.UpdatedISO}>{w.Updated}</time>],
                  ["Last refresh", w.LastRefresh && <time dateTime={w.LastRefreshISO}>{w.LastRefresh}</time>],
                  ["Access token expires", w.Expires && <time dateTime={w.ExpiresISO}>{w.Expires}</time>],
                ]}
              />
              {!w.ClientConfigured ? (
                <div className="space-y-2 text-xs text-muted">
                  <p>Register the app first. Set these in <code>.env</code> (or <code>config.yaml</code>) and restart the assistant:</p>
                  <pre className="overflow-x-auto border bg-surface-secondary p-3 font-mono text-[11px] text-foreground">
                    {"SPOTIFY_CLIENT_ID=<your app's client ID>\nSPOTIFY_REDIRECT_URI=https://<pi-name>.<tailnet>.ts.net/spotify/callback"}
                  </pre>
                </div>
              ) : !w.RedirectConfigured ? (
                <p className="text-xs text-muted">
                  The client ID is configured but the redirect URI is not. Set <code>spotify.redirect_uri</code> to the exact callback URL and restart.
                </p>
              ) : (
                <div className="space-y-2 text-xs">
                  <div className="text-muted">Redirect URI (must be in the app's allowlist exactly)</div>
                  <pre className="overflow-x-auto border bg-surface-secondary p-3 font-mono text-[11px]">{w.RedirectURI}</pre>
                  {!!data.Scopes?.length && (
                    <div className="flex flex-wrap gap-1">
                      {data.Scopes.map((s) => (
                        <code key={s} className="border px-1.5 py-0.5 text-[11px] text-muted">
                          {s}
                        </code>
                      ))}
                    </div>
                  )}
                </div>
              )}
              <div className="flex flex-wrap gap-2 pt-1">
                {w.CanConnect && <PostButton action="/spotify/connect" variant="primary">{w.Set ? "Reconnect Web API" : "Connect Web API"}</PostButton>}
                {w.CanVerify && <PostButton action="/spotify/verify">Verify stored token</PostButton>}
                {w.CanDisconnect && <PostButton action="/spotify/disconnect" variant="danger-soft">Disconnect</PostButton>}
              </div>
              <p className="text-[11px] text-subtle">
                The token is also listed on the Secrets page as <code>SPOTIFY_REFRESH_TOKEN</code>.
              </p>
            </div>
          </Section>
        </div>
      )}
      {!data && !error && <Empty>Loading…</Empty>}
    </PageFrame>
  );
}
