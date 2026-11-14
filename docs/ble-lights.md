# BLE lights (experimental)

Disabled by default. Settings → Bluetooth lights enables `lights.enabled`, scans,
and saves named devices with a protocol choice into `lights.devices` in the
assistant's existing SQLite settings store. These controllers normally need no
OS-level pairing: close the phone app, scan, select the appropriate protocol and
save. Forget removes only the saved selection; it never unpairs host devices.

The four `lights.*` Tools accept a saved device name/address and a value. An
omitted device is accepted only with exactly one saved light. Fast path commands
currently target that single light; use the Agent path for a named device when
several are saved. Colours accept #RRGGBB; voice extraction also knows red,
green, blue and white. Modes are rainbow, red pulse and strobe.

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
helper together have a seven-second deadline; discovery lasts three seconds.
Health reports opt-in and the last transport result, never inferred power or
colour. A successful GATT write does not prove the strip changed.

Compose mounts only the host system bus socket. No bluetoothd, privileged mode,
HCI reset, adapter power change, pairing removal or speaker configuration change
is introduced. The host must already run BlueZ and allow the configured
assistant UID to call org.bluez (host D-Bus policy may require its bluetooth
group). Do not loosen the whole system bus policy to solve an access failure.

## Owner hardware checks before enabling permanently

- Confirm the container UID's BlueZ access and an adapter capable of concurrent
  BLE and the existing bluez audio sink; scan while music plays.
- Identify the actual controller variant, save one of each protocol, restart
  the assistant, and confirm the saved selections survive.
- Test on/off, RGB primaries, brightness 0/50/100 and all three modes through
  Settings and assistant requests. Check multiple-device Agent selection.
- Unplug a strip: confirm bounded failure, truthful Health, responsive voice
  requests, and recovery when it returns. Close the vendor phone app first.
- Check the speaker keeps playing through discovery, writes and reconnects.
- Disable lights and confirm neither scans nor writes reach BlueZ.

No hardware, speaker coexistence or Pi deployment was tested during implementation.
