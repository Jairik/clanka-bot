import { useEffect, useRef, useState } from "react";
import { Button, Switch } from "@heroui/react";
import { getJSON } from "../api";
import { SelectField } from "../forms";
import { PageFrame } from "../shell";
import { Empty, Lede, Notice, Range, Section } from "../ui";

type Device = { address: string; name: string; protocol: string };
type View = { enabled: boolean; devices: Device[]; status: string };

const PROTOCOLS = [
  { value: "lotus", label: "Lotus Lantern" },
  { value: "happy", label: "HappyLighting" },
];

const MODES = [
  { value: "rainbow", label: "Rainbow" },
  { value: "pulse", label: "Pulse" },
  { value: "strobe", label: "Strobe" },
];

const protocolLabel = (protocol: string) => PROTOCOLS.find((item) => item.value === protocol)?.label ?? protocol;

// postLights sends one dashboard action. Scan answers with nearby devices;
// every other action answers {ok:true} and the caller reloads the view.
async function postLights(body: Record<string, unknown>): Promise<Response> {
  return fetch("/api/ui/lights", {
    method: "POST",
    headers: { "Content-Type": "application/json", Accept: "application/json" },
    credentials: "same-origin",
    body: JSON.stringify(body),
  });
}

// LightsPage is the Bluetooth controller page: opt in, scan, save, then
// command each saved light. Physical state stays unverified.
export function LightsPage() {
  const [view, setView] = useState<View>();
  const [found, setFound] = useState<Device[]>([]);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const [protocol, setProtocol] = useState("lotus");

  const load = async () => {
    setView(await getJSON<View>("/api/ui/lights"));
  };

  useEffect(() => {
    load().catch((e) => setError(e instanceof Error ? e.message : String(e)));
  }, []);

  // act runs one lights action, then refreshes the saved view. A scan also
  // keeps the nearby list so the user can save a controller.
  const act = async (action: string, extra: object = {}) => {
    setBusy(true);
    setError("");
    try {
      const response = await postLights({ action, ...extra });
      if (!response.ok) throw new Error((await response.text()).trim() || response.statusText);
      if (action === "scan") setFound(await response.json());
      await load();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  const enabled = view?.enabled ?? false;
  const devices = view?.devices ?? [];
  const status = !view ? "" : enabled ? view.status || "Ready to scan" : "Disabled";

  return (
    <PageFrame id="lights">
      <Notice error={error} />
      <div className="space-y-2.5">
        <Section title="Bluetooth" summary={view ? (enabled ? "On" : "Off") : ""} defaultOpen flush>
          <Lede>
            Experimental. Enable lights before scanning. A successful command does not prove the strip changed. HappyLighting brightness sets grayscale RGB.
          </Lede>
          <div className="space-y-2 px-3.5 py-3">
            <Switch isSelected={enabled} isDisabled={busy || !view} onChange={(on) => void act("enable", { value: String(on) })}>
              <Switch.Content className="flex w-full items-center justify-between gap-3">
                <span className="text-sm">Enable lights</span>
                <Switch.Control>
                  <Switch.Thumb />
                </Switch.Control>
              </Switch.Content>
            </Switch>
            <p role="status" className="text-xs text-muted">
              {status}
            </p>
          </div>
          <div className="flex flex-wrap items-end gap-3 px-3.5 pb-3">
            <SelectField label="Controller protocol" value={protocol} options={PROTOCOLS} isDisabled={busy} onChange={setProtocol} className="sm:w-64" />
            <Button variant="secondary" isDisabled={busy || !enabled} onPress={() => void act("scan")}>
              Scan for lights
            </Button>
          </div>
        </Section>

        {found.length > 0 && (
          <Section title="Nearby" summary={`${found.length}`} defaultOpen flush>
            <ul className="divide-y divide-separator">
              {found.map((device) => {
                const saved = devices.some((item) => item.address === device.address);
                return (
                  <li key={device.address} className="flex items-center justify-between gap-3 px-3.5 py-3">
                    <div className="min-w-0">
                      <p className="truncate text-sm">{device.name}</p>
                      <p className="truncate font-mono text-[11px] text-muted">{device.address}</p>
                    </div>
                    <Button
                      size="sm"
                      variant="secondary"
                      isDisabled={busy || saved}
                      onPress={() => void act("save", { devices: [...devices, { ...device, protocol }] })}
                    >
                      {saved ? "Saved" : "Save light"}
                    </Button>
                  </li>
                );
              })}
            </ul>
          </Section>
        )}

        <Section title="Saved" summary={`${devices.length}`} defaultOpen flush>
          {devices.length ? (
            <ul className="divide-y divide-separator">
              {devices.map((device) => (
                <SavedLight
                  key={device.address}
                  device={device}
                  busy={busy}
                  enabled={enabled}
                  onAct={act}
                  onForget={() => void act("save", { devices: devices.filter((item) => item.address !== device.address) })}
                />
              ))}
            </ul>
          ) : (
            <Empty>No lights saved. Scan, pick a protocol, and save a controller.</Empty>
          )}
        </Section>
      </div>
    </PageFrame>
  );
}

// SavedLight is one remembered controller: power, brightness, colour and mode.
function SavedLight({
  device,
  busy,
  enabled,
  onAct,
  onForget,
}: {
  device: Device;
  busy: boolean;
  enabled: boolean;
  onAct: (action: string, extra?: object) => Promise<void>;
  onForget: () => void;
}) {
  const locked = busy || !enabled;
  return (
    <li className="space-y-4 px-3.5 py-3">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="min-w-0">
          <p className="truncate text-sm">{device.name}</p>
          <p className="truncate font-mono text-[11px] text-muted">
            {protocolLabel(device.protocol)} · {device.address}
          </p>
        </div>
        <div className="flex flex-wrap gap-2">
          {["on", "off"].map((value) => (
            <Button key={value} size="sm" variant="secondary" isDisabled={locked} aria-label={`${device.name} ${value}`} onPress={() => void onAct("power", { device: device.address, value })}>
              {value === "on" ? "On" : "Off"}
            </Button>
          ))}
          <Button size="sm" variant="ghost" isDisabled={busy} aria-label={`Forget ${device.name}`} onPress={onForget}>
            Forget
          </Button>
        </div>
      </div>
      <div className="flex flex-wrap items-end gap-5">
        <Brightness name={device.name} disabled={locked} onCommit={(value) => void onAct("brightness", { device: device.address, value })} />
        <label className="flex items-center gap-2 text-xs text-muted">
          Colour
          <input
            type="color"
            aria-label={`${device.name} colour`}
            defaultValue="#ffffff"
            disabled={locked}
            className="h-8 w-10 cursor-pointer border border-border bg-transparent disabled:cursor-not-allowed disabled:opacity-50"
            onChange={(event) => void onAct("color", { device: device.address, value: event.target.value })}
          />
        </label>
        <div className="flex flex-wrap gap-2">
          {MODES.map((mode) => (
            <Button
              key={mode.value}
              size="sm"
              variant="secondary"
              isDisabled={locked}
              aria-label={`${device.name} ${mode.label}`}
              onPress={() => void onAct("mode", { device: device.address, value: mode.value })}
            >
              {mode.label}
            </Button>
          ))}
        </div>
      </div>
    </li>
  );
}

// Brightness commits after the thumb settles. Each value opens a Bluetooth
// connection, so a drag must not send a write per step.
function Brightness({ name, disabled, onCommit }: { name: string; disabled: boolean; onCommit: (value: string) => void }) {
  const [value, setValue] = useState(100);
  const timer = useRef<ReturnType<typeof setTimeout>>(undefined);
  useEffect(() => () => clearTimeout(timer.current), []);
  const change = (next: number) => {
    if (disabled) return;
    setValue(next);
    clearTimeout(timer.current);
    timer.current = setTimeout(() => onCommit(String(next)), 400);
  };
  return (
    <div className="min-w-40 flex-1">
      <div className="mb-1 flex justify-between text-xs text-muted">
        <span>Brightness</span>
        <span className="tabular-nums">{value}</span>
      </div>
      <Range label={`${name} brightness`} value={value} onChange={change} />
    </div>
  );
}
