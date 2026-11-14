# Monster lights

The Dashboard has two lighting surfaces:

- **Bluetooth** controllers on **Lights** (`/lights`, see `docs/ble-lights.md`).
- **Monster Smart Lighting** on **Monster lights** (`/monster`): the Wi-Fi
  devices controlled by the Monster Smart Lighting app.

They are separate because the hardware is separate. A Monster Duo+ monitor
light bar (`MNW7-2027-ICW` or `MLB7-2027-WW`) is a **Wi-Fi + BLE** device. Its
BLE radio exists only to hand the household Wi-Fi credentials to the bar during
onboarding; every later command goes through the Monster/Ayla cloud. Scanning
for it on the Bluetooth page will find nothing useful, and no GATT write can
turn it on.

## Setup

The bar must already be added to the Monster Smart Lighting app and online on
the 2.4 GHz network. Then:

1. Open the Dashboard's **Secrets** page.
2. Set `MONSTER_EMAIL` and `MONSTER_PASSWORD` to the same account the Monster
   app uses. Both are write-only: the API reports only set/last-four/source,
   and neither appears in a setting, log line or error.
3. Open **Monster lights**. The account's devices are listed with their last
   cloud state.

`MONSTER_EMAIL` and `MONSTER_PASSWORD` can also be bootstrapped from `.env`;
a value stored on the Secrets page wins, matching every other secret in the
assistant.

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

## Verification

```sh
cd assistant
go test ./internal/monster ./tools/monster ./internal/dashboard
```

The tests drive the full sign-in chain against local fake hosts, prove token
caching and the one-401 retry, map the device properties, and pin the
dashboard's validation and error states. A manual reachability check of the
real endpoints is a 401 without an account:

```sh
curl -s -o /dev/null -w '%{http_code}\n' -X POST \
  https://api.monstergen2.bycopilot.com/v4/auth/login \
  -H 'Content-Type: application/json' \
  --data '{"authenticationDetails":{"applicationId":"MONSTERGEN2","email":"nobody@example.invalid","password":"x"},"deviceDetails":{"applicationVersion":"1.0.0","deviceId":"00000000-0000-4000-8000-000000000000","deviceModel":"HomeAssistant","deviceType":"phone","osType":"android","osVersion":"1","timezone":{"currentTimeMillis":0,"offsetMillis":0,"timezoneId":"UTC"}}}'
# 401
```
