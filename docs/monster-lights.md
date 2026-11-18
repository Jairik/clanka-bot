# Monster lights

The Dashboard has one lighting page, **Lights** (`/lights`), with two sections:

- **Room lights**: the **Bluetooth** controllers, see `docs/ble-lights.md`.
- **PC / desk lights**: the Wi-Fi devices controlled by the Monster Smart
  Lighting app. The old `/monster` address still opens this page.

The sections are separate because the hardware is separate. A Monster Duo+
monitor light bar (`MNW7-2027-ICW` or `MLB7-2027-WW`) is a **Wi-Fi + BLE**
device. Its BLE radio exists only to hand the household Wi-Fi credentials to the
bar during onboarding; every later command goes through the Monster/Ayla cloud.
Scanning for it in the Bluetooth section will find nothing useful, and no GATT
write can turn it on. The two sections load independently: a cloud read that is
slow or failing never hides the Bluetooth controls.

## Setup

The bar must already be added to the Monster Smart Lighting app and online on
the 2.4 GHz network. Then:

1. Open the Dashboard's **Secrets** page.
2. Set `MONSTER_EMAIL` and `MONSTER_PASSWORD` to the same account the Monster
   app uses. Both are write-only: the API reports only set/last-four/source,
   and neither appears in a setting, log line or error. The password is kept
   exactly as entered, including leading and trailing spaces; only pasted line
   breaks and tabs are stripped.
3. Open **Lights** and find **PC / desk lights**. The account's devices are
   listed with their last cloud state.

`MONSTER_EMAIL` and `MONSTER_PASSWORD` can also be bootstrapped from `.env`,
where they are used verbatim; a value stored on the Secrets page wins, matching
every other secret in the assistant.

## What the page controls

| Control | Wire property | Notes |
| --- | --- | --- |
| On / Off | `power` | `1` / `0` |
| Brightness | `brightness` in white mode, `color_bright` in colour and scene modes | The device keeps two dimmers; the page writes the one the active mode uses. |
| White | `mode=white`, `color_temp` | 0-100 on the device maps to 2700-6500 K; 0 is warm. |
| Colour | `mode=color`, `color_select` | Packed `R<<16 \| G<<8 \| B`. |
| Scene | `mode` then `st_pat` / `dyn_pat` / `mus_pat` / `diy_pat` / `per_ic_pat` | The page lists the device's own populated slots, including renamed DIY scenes. |

A write is two Ayla datapoints when it changes mode (mode first, then the
value), and one when it does not. A successful cloud write does not prove the
bar changed; the next page load reads the cloud's reported state.

## How authentication works

The Monster app's public application identifiers are compiled in. The owner's
email and password are exchanged for:

1. a Copilot access token (`api.monstergen2.bycopilot.com/v4/auth/login`),
2. a partner ticket (`sphere.bycopilot.com/v2/partner/<id>/acquire_ticket`),
3. an Ayla session token (`user-field.aylanetworks.com/api/v1/token_sign_in`).

The Ayla token stays in memory, is refreshed five minutes before its stated
expiry, and is replaced once after a 401. Credentials and tokens are never
logged; an upstream response body is never copied into an error, so a token
echoed by a failure cannot reach the page or the log.

A failed sign-in is remembered instead of retried on every read: a rejected
password returns the same error for five minutes, and an upstream or network
failure for thirty seconds, with no request to the cloud in between. Saving a
different password clears that at once. This keeps a page load and a health poll
from hammering a consumer login that rate-limits or locks accounts after
repeated failures.

The sign-in runs on its own deadline, not the caller's. A health poll that runs
out of its shared budget returns its timeout while the three POSTs keep going
and cache the token for the next poll, so a slow but working account is not
stuck reporting "did not answer in time".

The password in the Copilot login body is the resolved secret unchanged, along
the whole path: the Secrets page stores it with surrounding spaces intact
(stripping only pasted `\r`, `\n` and tabs) and an `.env` value is used
verbatim; the module's `Credentials` closure returns the stored value
untouched; and `Service.credentials` trims only the email before both values
go into the login request. A password that starts or ends with a space
therefore signs in, whichever source supplied it.

Endpoints, the sign-in chain and the property names follow the
[MIT-licensed `Aareon/monster-rgbic-ha` integration](https://github.com/Aareon/monster-rgbic-ha),
which reverse-engineered them from the Android app and verified them on
hardware.

## Limits

- **Cloud only.** The Ayla LAN protocol needs each device's `lanip_key` from
  the account plus a callback HTTP port the device can reach. Neither is
  implemented here; the page requires internet access to Monster's cloud.
- **Monster Smart Lighting only.** The older Monster Illuminessence / Monster
  Smart (Tuya-based) products are a different platform and are not supported.
- **Onboarding is not implemented.** Pair a new bar in the Monster app first;
  this page lists what the account already owns.
- The cloud reports state asynchronously. A write can take a second or two to
  appear on the next read; the page does not pretend the physical bar changed.

## Voice control

Voice control is exposed to both Fast and Agent paths through five Tools in `tools/monster`:

| Tool | Permission | Values | Notes |
| --- | --- | --- | --- |
| `monster.power` | `act` | `on` / `off` | Turns the bar on or off. |
| `monster.brightness` | `act` | `0`–`100` | Drives white or colour dimmer according to active mode. |
| `monster.color` | `act` | `#RRGGBB` or color names | Writes `mode=color` then `color_select`. |
| `monster.white` | `act` | `2000`–`7000` kelvin, or `warm`, `cool`, `daylight` | Writes `mode=white` then `color_temp`. |
| `monster.scene` | `act` | scene name or slot ID | Resolves scene names through namematch over the device's own scenes. |

All five Tools are `PermissionAct` and `LocalOnlyNo`. Spoken outputs are terse acknowledgements (`"Done."`, or the group line below).

A general lighting command also reaches these lights. "Turn the lights off",
"turn on all lights" and the omitted-device form of `lights.power`,
`lights.brightness` and `lights.color` write every saved Bluetooth controller
**and** every light on the Monster account. The two transports run at the same
time and each keeps its own budget (fourteen seconds for the BLE batch, eight
for the cloud), so slowness or failure on one side cannot block the other. A
partial result is spoken, for example "Light command sent. The PC light did not
answer."; an account that was never set up is skipped without a word. Modes stay
Bluetooth-only, and white and scenes stay on the `monster.*` Tools.

### Device resolution

Device resolution matches spoken device names case-insensitively and through `namematch` against the cloud device names on the account. To avoid adding an extra network round trip per voice command, `Service` caches the device list for 60 seconds.

A generic target — `"pc light"`, `"pc lights"`, `"monitor light"`, `"light bar"`, `"duo"` or an omitted device — resolves the account's one bar-like device by name or model when the account also holds strips or bulbs. When exactly one device is configured it is that device. Two bar-like devices stay ambiguous and ask which one, rather than guessing. The Fast path's name lookup for extraction gets its own two-second budget, so a cold sign-in cannot spend the Tool's deadline before the write starts.

### Routing separation

Routing distinguishes between the group and one specific light:
- Plural `"lights"` phrases (e.g. `"turn the lights on"`, `"dim the lights to 20 percent"`) stay with `lights.*`, which fans a general command out to both transports.
- PC phrasing (`"pc light"`, `"monitor light"`, `"light bar"`, `"duo"`, a cloud device name) and the operations only the bar has (`"warm white"`, a scene name) stay with `monster.*`.

`tools/lights` asks the Monster module through the small `internal/lighting`
seam whether an utterance names the PC light, so the two transports cannot drift
apart on who owns `"light bar"`. A saved Bluetooth controller literally called
"Light Bar" still wins its exact name over that alias.

## Verification

```sh
cd assistant
go test ./internal/lighting ./internal/monster ./tools/monster ./tools/lights ./internal/dashboard
```

The tests drive the full sign-in chain against local fake hosts, prove token
caching, the one-401 retry, the failure backoff, a sign-in that outlives its
caller's deadline, and that a space-padded password is sent unchanged; they also
map the device properties, resolve the bar among strips, and pin the dashboard's
validation and error states. `tools/lights` proves the group fan-out: both
transports together, either transport alone when the other is absent or empty,
partial batches, and the combined spoken reply.
A manual reachability check of the real endpoints is a 401 without an account:

```sh
curl -s -o /dev/null -w '%{http_code}\n' -X POST \
  https://api.monstergen2.bycopilot.com/v4/auth/login \
  -H 'Content-Type: application/json' \
  --data '{"authenticationDetails":{"applicationId":"MONSTERGEN2","email":"nobody@example.invalid","password":"x"},"deviceDetails":{"applicationVersion":"1.0.0","deviceId":"00000000-0000-4000-8000-000000000000","deviceModel":"HomeAssistant","deviceType":"phone","osType":"android","osVersion":"1","timezone":{"currentTimeMillis":0,"offsetMillis":0,"timezoneId":"UTC"}}}'
# 401
```
