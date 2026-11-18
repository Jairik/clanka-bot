# BLE lights (experimental)

Disabled by default. The Lights page enables `lights.enabled`, scans,
and saves named devices with a protocol choice into `lights.devices` in the
assistant's existing SQLite settings store. These controllers normally need no
OS-level pairing: close the phone app, scan, select the appropriate protocol and
save. Forget removes only the saved selection; it never unpairs host devices.

Monster Smart Lighting Wi-Fi devices (the Duo+ monitor light bar, RGBIC
strips and bulbs) are not BLE-controlled: their Bluetooth radio only hands
over Wi-Fi credentials during onboarding. They appear on the same Lights page
under **PC / desk lights**, because one page owns every light; see
`docs/monster-lights.md`.

The four `lights.*` Tools accept a saved device name, an address, or `all`. An
omitted device and `all` both mean every light, and for power, brightness and
colour that includes the PC / desk Monster light: the two transports run at the
same time, each under its own budget, so a slow cloud call cannot delay a BLE
write. A mode is Bluetooth-only, and white and scenes stay on `monster.*`. A
command that names the PC light ("pc light", "monitor light", "light bar", in
singular or plural) never reaches a saved controller: it stays with the Monster
Tools. Names resolve through the shared name matcher, so case, punctuation and
mild speech-to-text slips need no exact spelling; an ambiguous name is refused
rather than guessed. `lights.list` returns saved names and protocols (never
addresses) so the Agent can choose a device; it does not list cloud lights,
because they are not saved controllers. Fast path commands fill the device from
the utterance: a saved name selects that light, plural "lights" targets the
whole group, a singular "the light" with one saved controller names that
controller (so it never touches the PC bar), and a singular unnamed light with
several saved controllers stays on the Agent path. A batch shares one
fourteen-second write budget, so the static Tool timeout does not grow with the
device count; a partial failure reports "Done for 2 of 3 lights" and Health
names the controller that did not answer. A group command says which side fell
short ("Light command sent. The PC light did not answer."), and a Monster
account that was never set up is skipped silently. Colours accept #RRGGBB; voice
extraction also knows red, green, blue and white. Modes are rainbow, red pulse
and strobe.

## Protocol references and limits

- Lotus Lantern: [ELK-BLEDOM community commands and characteristic FFF3](https://github.com/dave-code-ruiz/elkbledom/blob/master/custom_components/elkbledom/models.json)
  and [effect IDs](https://github.com/dave-code-ruiz/elkbledom/blob/master/custom_components/elkbledom/definitions.json).
  This implements the zero-prefix `7e 00 … ef` ELK-BLEDOM variant. Other
  firmware variants (including the `7e 07` version) need a separate driver;
  a matching advertised name alone is no guarantee of compatibility.
- HappyLighting: [Triones byte formats, FFD9 and built-in modes](https://github.com/madhead/saberlight/blob/master/protocols/Triones/protocol.md)
  and [Python implementation](https://github.com/DmitriyBukreev/trionesDevice).
  Brightness sends grayscale RGB because this protocol has no independent RGB
  brightness command; it replaces the selected colour and exits an effect.

A Driver supplies command bytes and a characteristic. The transport is an
embedded Python helper with pinned Bleak, using host BlueZ over D-Bus. Each
write opens and closes its own connection and retries once. Queueing and the
helper together have a fourteen-second deadline, long enough for a cold
ELK-BLEDOM connect; discovery lasts three seconds.
Health reports opt-in and the last transport result for the named controller,
including the helper's reason (for example
`ELK-BLEDOB04: failed to discover services, device disconnected`), never
inferred power or colour. A successful GATT write does not prove the strip
changed. One saved light that stops answering keeps that row red until a later
write succeeds, so "Forget" a controller that is gone and power-cycle one that
the vendor phone app still holds.

Compose mounts only the host system bus socket. No bluetoothd, privileged mode,
HCI reset, adapter power change, pairing removal or speaker configuration change
is introduced. The mount itself is not Bluetooth-scoped: it exposes whatever the
host bus policy allows the assistant UID, and its trust boundary is documented
in [deploy.md](deploy.md#container-privileges-and-the-system-d-bus-grant). The
host must already run BlueZ and allow the configured assistant UID to call
org.bluez (host D-Bus policy may require its bluetooth group). Do not loosen the
whole system bus policy to solve an access failure.

## Owner hardware checks before enabling permanently

- Confirm the container UID's BlueZ access and an adapter capable of concurrent
  BLE and the existing bluez audio sink; scan while music plays.
- Identify the actual controller variant, save one of each protocol, restart
  the assistant, and confirm the saved selections survive.
- Test on/off, RGB primaries, brightness 0/50/100 and all three modes through
  the Lights page and assistant requests. Check multiple-device Agent selection.
- Unplug a strip: confirm bounded failure, truthful Health, responsive voice
  requests, and recovery when it returns. Close the vendor phone app first.
- Check the speaker keeps playing through discovery, writes and reconnects.
- Disable lights and confirm neither scans nor writes reach BlueZ.

No hardware, speaker coexistence or Pi deployment was tested during implementation.
