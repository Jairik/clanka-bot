import { useEffect, useRef, useState } from "react";
import { Button, Switch } from "@heroui/react";
import { usePageData } from "../api";
import { SelectField, useActions } from "../forms";
import { PageFrame } from "../shell";
import { Empty, Lede, Notice, Range, Section } from "../ui";
import type { LightsDevice, LightsView } from "../types";

// Writes are JSON, not the form encoding the other pages use.
const JSON_INIT: RequestInit = { headers: { "Content-Type": "application/json", Accept: "application/json" } };

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

// LightsPage is the Bluetooth controller page: opt in, scan, save, then
// command each saved light. Physical state stays unverified.
export function LightsPage() {
  const { data: view, error, reload } = usePageData("lights");
  const [found, setFound] = useState<LightsDevice[]>([]);
  const [protocol, setProtocol] = useState("lotus");
  const actions = useActions<LightsView>((written) => {
    // A scan answers with the nearby-device list instead of the page view;
    // every other JSON write answers {ok:true}.
    if (Array.isArray(written)) setFound(written as unknown as LightsDevice[]);
  });
  const busy = !!actions.pending;

  // act runs one lights action, then refreshes the saved view. A scan also
  // keeps the nearby list so the user can save a controller.
  const act = async (action: string, extra: object = {}) => {
    await actions.run("/api/ui/lights", JSON.stringify({ action, ...extra }), JSON_INIT);
    await reload();
  };

  const enabled = view?.enabled ?? false;
  const devices = view?.devices ?? [];
  const status = !view ? "" : enabled ? view.status || "Ready to scan" : "Disabled";

  return (
    <PageFrame id="lights">
      <Notice error={error || actions.error} />
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
  device: LightsDevice;
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
