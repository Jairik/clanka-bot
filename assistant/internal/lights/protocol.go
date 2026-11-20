// Package lights controls write-only BLE controllers without owning the adapter.
package lights

import (
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

// Driver encodes a platform command; future platforms can implement this seam.
type Driver interface {
	Encode(operation, value string) ([]byte, error)
	Characteristic() string
}

// BLEDriver supports the Lotus Lantern and HappyLighting wire formats.
// Color is the last commanded #RRGGBB. HappyLighting brightness scales it;
// empty means no colour is known, so brightness stays grey.
type BLEDriver struct {
	Protocol string
	Color    string
}

func (d BLEDriver) Characteristic() string {
	if d.Protocol == "happy" {
		return "0000ffd9-0000-1000-8000-00805f9b34fb"
	}
	return "0000fff3-0000-1000-8000-00805f9b34fb"
}

// Encode follows https://github.com/dave-code-ruiz/elkbledom/blob/master/custom_components/elkbledom/models.json and https://github.com/DmitriyBukreev/trionesDevice .
func (d BLEDriver) Encode(op, value string) ([]byte, error) {
	if d.Protocol != "lotus" && d.Protocol != "happy" {
		return nil, fmt.Errorf("choose lotus or happy")
	}
	happy := d.Protocol == "happy"
	switch op {
	case "power":
		if value != "on" && value != "off" {
			break
		}
		if happy {
			v := byte(0x24)
			if value == "on" {
				v = 0x23
			}
			return []byte{0xcc, v, 0x33}, nil
		}
		v := byte(0)
		on := byte(0)
		if value == "on" {
			v = 0xf0
			on = 1
		}
		return []byte{0x7e, 0, 4, v, 0, on, 0xff, 0, 0xef}, nil
	case "color":
		rgb, err := hex.DecodeString(strings.TrimPrefix(value, "#"))
		if err != nil || len(rgb) != 3 {
			break
		}
		if happy {
			return []byte{0x56, rgb[0], rgb[1], rgb[2], 0, 0xf0, 0xaa}, nil
		}
		return []byte{0x7e, 0, 5, 3, rgb[0], rgb[1], rgb[2], 0, 0xef}, nil
	case "brightness":
		n, err := strconv.Atoi(value)
		if err != nil || n < 0 || n > 100 {
			break
		}
		if happy {
			// This protocol's only level control is another RGB write.
			return happyBrightness(n, d.Color)
		}
		return []byte{0x7e, 0, 1, byte(n), 0xff, 0, 0xff, 0, 0xef}, nil
	case "mode":
		modes := map[string]byte{"rainbow": 0x25, "pulse": 0x26, "strobe": 0x30}
		if !happy {
			modes = map[string]byte{"rainbow": 0x88, "pulse": 0x8b, "strobe": 0x96}
		}
		v, ok := modes[value]
		if !ok {
			break
		}
		if happy {
			return []byte{0xbb, v, 0x10, 0x44}, nil
		}
		return []byte{0x7e, 0, 3, v, 3, 0, 0, 0, 0xef}, nil
	}
	return nil, fmt.Errorf("invalid %s value", op)
}

// happyBrightness encodes a HappyLighting brightness level.
// With no colour it sends grey, which is what the controller already did.
// With a colour it sends that colour scaled by percent/100.
func happyBrightness(percent int, colour string) ([]byte, error) {
	if colour == "" {
		v := byte(percent * 255 / 100)
		return []byte{0x56, v, v, v, 0, 0xf0, 0xaa}, nil
	}
	rgb, err := hex.DecodeString(strings.TrimPrefix(colour, "#"))
	if err != nil || len(rgb) != 3 {
		return nil, fmt.Errorf("invalid color value")
	}
	for i, channel := range rgb {
		rgb[i] = scaleColorChannel(channel, percent)
	}
	return []byte{0x56, rgb[0], rgb[1], rgb[2], 0, 0xf0, 0xaa}, nil
}

// scaleColorChannel rounds channel * percent / 100.
// A non-zero channel stays at least 1 when the level is not zero, so 1% is not black.
// Zero percent is black on every channel.
func scaleColorChannel(channel byte, percent int) byte {
	if percent == 0 || channel == 0 {
		return 0
	}
	// Adding 50 before dividing rounds half up.
	scaled := (int(channel)*percent + 50) / 100
	if scaled < 1 {
		scaled = 1
	}
	if scaled > 255 {
		return 255
	}
	return byte(scaled)
}

// canonicalColor returns the colour as #rrggbb.
func canonicalColor(value string) (string, error) {
	rgb, err := hex.DecodeString(strings.TrimPrefix(strings.TrimSpace(value), "#"))
	if err != nil || len(rgb) != 3 {
		return "", fmt.Errorf("invalid color value")
	}
	return "#" + hex.EncodeToString(rgb), nil
}
