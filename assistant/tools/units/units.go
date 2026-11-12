// Package units implements units.convert for common length, mass, volume,
// temperature, time, speed and area units. The table is fixed in code: no
// currency, no live rates and no network lookup.
package units

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// Name is the canonical units.convert Tool name.
const Name = "units.convert"

const (
	// maxInput keeps a spoken value in a range where a conversion is useful
	// and every float operation stays finite.
	maxInput  = 1e15
	maxResult = 1e15
)

// dimension is the kind of quantity a unit measures. A conversion is allowed
// only within one dimension; temperature is affine, so it has its own entry
// points in convert.
type dimension uint8

const (
	dimLength dimension = iota + 1
	dimMass
	dimVolume
	dimTemperature
	dimTime
	dimSpeed
	dimArea
)

// unitDef is one canonical unit. factor is the multiplier to the dimension's
// base unit (meter, kilogram, liter, second, meter per second, square meter).
// Temperature ignores factor and uses the affine formulas in convert.
type unitDef struct {
	symbol   string
	singular string
	plural   string
	dim      dimension
	factor   float64
}

// unitDefs is the closed conversion table. Keep factors as exact written
// constants so tests can assert known literals independently.
var unitDefs = map[string]unitDef{
	// Length, base meter.
	"mm":  {"mm", "millimeter", "millimeters", dimLength, 0.001},
	"cm":  {"cm", "centimeter", "centimeters", dimLength, 0.01},
	"m":   {"m", "meter", "meters", dimLength, 1},
	"km":  {"km", "kilometer", "kilometers", dimLength, 1000},
	"in":  {"in", "inch", "inches", dimLength, 0.0254},
	"ft":  {"ft", "foot", "feet", dimLength, 0.3048},
	"yd":  {"yd", "yard", "yards", dimLength, 0.9144},
	"mi":  {"mi", "mile", "miles", dimLength, 1609.344},
	"nmi": {"nmi", "nautical mile", "nautical miles", dimLength, 1852},

	// Mass, base kilogram.
	"mg": {"mg", "milligram", "milligrams", dimMass, 0.000001},
	"g":  {"g", "gram", "grams", dimMass, 0.001},
	"kg": {"kg", "kilogram", "kilograms", dimMass, 1},
	"t":  {"t", "tonne", "tonnes", dimMass, 1000},
	"oz": {"oz", "ounce", "ounces", dimMass, 0.028349523125},
	"lb": {"lb", "pound", "pounds", dimMass, 0.45359237},
	"st": {"st", "stone", "stones", dimMass, 6.35029318},

	// Volume, base liter.
	"ml":   {"ml", "milliliter", "milliliters", dimVolume, 0.001},
	"l":    {"l", "liter", "liters", dimVolume, 1},
	"cup":  {"cup", "cup", "cups", dimVolume, 0.2365882365},
	"pt":   {"pt", "pint", "pints", dimVolume, 0.473176473},
	"qt":   {"qt", "quart", "quarts", dimVolume, 0.946352946},
	"gal":  {"gal", "gallon", "gallons", dimVolume, 3.785411784},
	"floz": {"floz", "fluid ounce", "fluid ounces", dimVolume, 0.0295735295625},
	"tbsp": {"tbsp", "tablespoon", "tablespoons", dimVolume, 0.01478676478125},
	"tsp":  {"tsp", "teaspoon", "teaspoons", dimVolume, 0.00492892159375},

	// Temperature, affine around Celsius.
	"C": {"C", "degree Celsius", "degrees Celsius", dimTemperature, 1},
	"F": {"F", "degree Fahrenheit", "degrees Fahrenheit", dimTemperature, 1},
	"K": {"K", "kelvin", "kelvin", dimTemperature, 1},

	// Time, base second.
	"s":    {"s", "second", "seconds", dimTime, 1},
	"min":  {"min", "minute", "minutes", dimTime, 60},
	"h":    {"h", "hour", "hours", dimTime, 3600},
	"day":  {"day", "day", "days", dimTime, 86400},
	"week": {"week", "week", "weeks", dimTime, 604800},

	// Speed, base meter per second.
	"mps":  {"mps", "meter per second", "meters per second", dimSpeed, 1},
	"kph":  {"kph", "kilometer per hour", "kilometers per hour", dimSpeed, 1.0 / 3.6},
	"mph":  {"mph", "mile per hour", "miles per hour", dimSpeed, 0.44704},
	"knot": {"knot", "knot", "knots", dimSpeed, 0.5144444444444445},

	// Area, base square meter.
	"m2":   {"m2", "square meter", "square meters", dimArea, 1},
	"km2":  {"km2", "square kilometer", "square kilometers", dimArea, 1000000},
	"ha":   {"ha", "hectare", "hectares", dimArea, 10000},
	"acre": {"acre", "acre", "acres", dimArea, 4046.8564224},
	"sqft": {"sqft", "square foot", "square feet", dimArea, 0.09290304},
	"sqin": {"sqin", "square inch", "square inches", dimArea, 0.00064516},
}

// unitAliases maps spoken and common written forms to a unit symbol. The table
// is deliberately finite; an unknown word asks the user to rephrase instead of
// guessing a nearby unit.
var unitAliases = map[string]string{
	// Length.
	"mm": "mm", "millimeter": "mm", "millimeters": "mm", "millimetre": "mm", "millimetres": "mm",
	"cm": "cm", "centimeter": "cm", "centimeters": "cm", "centimetre": "cm", "centimetres": "cm",
	"m": "m", "meter": "m", "meters": "m", "metre": "m", "metres": "m",
	"km": "km", "kilometer": "km", "kilometers": "km", "kilometre": "km", "kilometres": "km",
	"in": "in", "inch": "in", "inches": "in",
	"ft": "ft", "foot": "ft", "feet": "ft",
	"yd": "yd", "yard": "yd", "yards": "yd",
	"mi": "mi", "mile": "mi", "miles": "mi",
	"nmi": "nmi", "nautical mile": "nmi", "nautical miles": "nmi",

	// Mass.
	"mg": "mg", "milligram": "mg", "milligrams": "mg",
	"g": "g", "gram": "g", "grams": "g", "gramme": "g", "grammes": "g",
	"kg": "kg", "kilogram": "kg", "kilograms": "kg", "kilo": "kg", "kilos": "kg",
	"t": "t", "tonne": "t", "tonnes": "t",
	"oz": "oz", "ounce": "oz", "ounces": "oz",
	"lb": "lb", "lbs": "lb", "pound": "lb", "pounds": "lb",
	"st": "st", "stone": "st", "stones": "st",

	// Volume.
	"ml": "ml", "milliliter": "ml", "milliliters": "ml", "millilitre": "ml", "millilitres": "ml",
	"l": "l", "liter": "l", "liters": "l", "litre": "l", "litres": "l",
	"cup": "cup", "cups": "cup",
	"pt": "pt", "pint": "pt", "pints": "pt",
	"qt": "qt", "quart": "qt", "quarts": "qt",
	"gal": "gal", "gallon": "gal", "gallons": "gal",
	"floz": "floz", "fl oz": "floz", "fluid ounce": "floz", "fluid ounces": "floz",
	"tbsp": "tbsp", "tablespoon": "tbsp", "tablespoons": "tbsp",
	"tsp": "tsp", "teaspoon": "tsp", "teaspoons": "tsp",

	// Temperature.
	"c": "C", "degree c": "C", "degrees c": "C", "degree celsius": "C", "degrees celsius": "C", "celsius": "C",
	"f": "F", "degree f": "F", "degrees f": "F", "degree fahrenheit": "F", "degrees fahrenheit": "F", "fahrenheit": "F",
	"k": "K", "kelvin": "K", "kelvins": "K",

	// Time.
	"s": "s", "sec": "s", "secs": "s", "second": "s", "seconds": "s",
	"min": "min", "mins": "min", "minute": "min", "minutes": "min",
	"h": "h", "hr": "h", "hrs": "h", "hour": "h", "hours": "h",
	"day": "day", "days": "day",
	"week": "week", "weeks": "week",

	// Speed.
	"mps": "mps", "meter per second": "mps", "meters per second": "mps", "metre per second": "mps", "metres per second": "mps",
	"kph": "kph", "kmh": "kph", "km/h": "kph", "kilometer per hour": "kph", "kilometers per hour": "kph",
	"kilometre per hour": "kph", "kilometres per hour": "kph",
	"mph": "mph", "mile per hour": "mph", "miles per hour": "mph",
	"knot": "knot", "knots": "knot",

	// Area.
	"m2": "m2", "square meter": "m2", "square meters": "m2", "square metre": "m2", "square metres": "m2", "sq m": "m2",
	"km2": "km2", "square kilometer": "km2", "square kilometers": "km2", "square kilometre": "km2",
	"square kilometres": "km2", "sq km": "km2",
	"ha": "ha", "hectare": "ha", "hectares": "ha",
	"acre": "acre", "acres": "acre",
	"sqft": "sqft", "square foot": "sqft", "square feet": "sqft", "sq ft": "sqft",
	"sqin": "sqin", "square inch": "sqin", "square inches": "sqin", "sq in": "sqin",
}

// In is the value and the two units to convert between.
type In struct {
	Value float64 `json:"value"`
	From  string  `json:"from"`
	To    string  `json:"to"`
}

// Out is the converted value. Display is the trimmed form Speak says.
type Out struct {
	Value   float64 `json:"value"`
	From    string  `json:"from"`
	To      string  `json:"to"`
	Result  float64 `json:"result"`
	Display string  `json:"display"`
}

// New builds the units.convert Tool.
func New() tool.Tool[In, Out] {
	return tool.Tool[In, Out]{
		Name:        Name,
		Description: "Convert a value between common units of length, mass, volume, temperature, time, speed or area.",
		Permission:  tool.PermissionRead,
		LocalOnly:   tool.LocalOnlyNo,
		Handler: func(_ context.Context, in In) (Out, error) {
			return convert(in)
		},
		Speak:   speak,
		Extract: extract,
		Examples: []string{
			"convert 5 miles to km",
			"what is 5 miles in kilometers",
			"how many kilometers in 5 miles",
			"convert 100 fahrenheit to celsius",
			"how many minutes in a day",
			"convert 1 kilogram to pounds",
			"what is 30 celsius in fahrenheit",
			"convert 2 gallons to liters",
			"how many feet in a mile",
			// One example per common dimension keeps the unit vocabulary in
			// the index instead of relying on a single length phrasing.
			"how many cups in a liter",
			"what is 72 fahrenheit in celsius",
			"how many ounces in a pound",
			"convert 60 mph to kph",
			"what is 1 acre in square meters",
			"convert 500 milliliters to cups",
			"how many inches in a foot",
			"convert 5 feet to meters",
		},
	}
}

// convert validates both units and the dimension before doing the arithmetic,
// so "miles to kilograms" is a clear error rather than a meaningless number.
func convert(in In) (Out, error) {
	from, ok := lookupUnit(in.From)
	if !ok {
		return Out{}, unknownUnit(in.From)
	}
	to, ok := lookupUnit(in.To)
	if !ok {
		return Out{}, unknownUnit(in.To)
	}
	if from.dim != to.dim {
		return Out{}, &tool.ToolError{Say: fmt.Sprintf("I can't convert %s to %s.", from.plural, to.plural)}
	}
	if math.IsNaN(in.Value) || math.IsInf(in.Value, 0) {
		return Out{}, &tool.ToolError{Say: "That value is not a finite number."}
	}
	if math.Abs(in.Value) > maxInput {
		return Out{}, &tool.ToolError{Say: "That value is too large to convert."}
	}
	result, err := convertValue(in.Value, from, to)
	if err != nil {
		return Out{}, err
	}
	return Out{
		Value:   in.Value,
		From:    from.symbol,
		To:      to.symbol,
		Result:  result,
		Display: formatNumber(result),
	}, nil
}

// convertValue turns a value into the dimension base and out again. Temperature
// is affine: Celsius is the base, so Fahrenheit subtracts 32 before scaling.
func convertValue(value float64, from, to unitDef) (float64, error) {
	var base float64
	if from.dim == dimTemperature {
		switch from.symbol {
		case "C":
			base = value
		case "F":
			base = (value - 32) * 5 / 9
		case "K":
			base = value - 273.15
		}
	} else {
		base = value * from.factor
	}

	var result float64
	if to.dim == dimTemperature {
		switch to.symbol {
		case "C":
			result = base
		case "F":
			result = base*9/5 + 32
		case "K":
			result = base + 273.15
		}
	} else {
		result = base / to.factor
	}
	if math.IsNaN(result) || math.IsInf(result, 0) || math.Abs(result) > maxResult {
		return 0, &tool.ToolError{Say: "That conversion is too large."}
	}
	return result, nil
}

// speak uses the long unit names so the reply reads naturally aloud.
func speak(out Out) string {
	from, _ := lookupUnit(out.From)
	to, _ := lookupUnit(out.To)
	return formatNumber(out.Value) + " " + unitName(from, out.Value) +
		" is " + out.Display + " " + unitName(to, out.Result) + "."
}

// unitName picks the singular or plural spoken name for a value.
func unitName(def unitDef, value float64) string {
	if math.Abs(value) == 1 {
		return def.singular
	}
	return def.plural
}

// unknownUnit keeps the message specific without pretending a nearby unit was
// meant.
func unknownUnit(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return &tool.ToolError{Say: "Tell me a value and two units to convert."}
	}
	return &tool.ToolError{Say: fmt.Sprintf("I don't know the unit %q. I can convert length, mass, volume, temperature, time, speed and area units.", name)}
}

// lookupUnit resolves one spoken or written unit name.
func lookupUnit(name string) (unitDef, bool) {
	normalized := normalizeUnit(name)
	if symbol, ok := unitAliases[normalized]; ok {
		return unitDefs[symbol], true
	}
	// A regular plural that is not listed still resolves to its singular.
	if strings.HasSuffix(normalized, "s") {
		if symbol, ok := unitAliases[strings.TrimSuffix(normalized, "s")]; ok {
			return unitDefs[symbol], true
		}
	}
	return unitDef{}, false
}

// normalizeUnit lowercases, removes degree marks and punctuation, and collapses
// spaces so "fl. oz." and "Fl Oz" both reach the table.
func normalizeUnit(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	name = strings.ReplaceAll(name, "°", "")
	replacer := strings.NewReplacer(".", "", ",", "", "\"", "", "'", "")
	name = replacer.Replace(name)
	return strings.Join(strings.Fields(name), " ")
}

// amountPattern reads the leading number of a source phrase.
var amountPattern = regexp.MustCompile(`^([-+]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+))\s*(.+)$`)

// amountAndUnit reads "5 miles" or "a day".
func amountAndUnit(phrase string) (float64, string, bool) {
	phrase = strings.TrimSpace(phrase)
	phrase = strings.TrimPrefix(phrase, "about ")
	phrase = strings.TrimPrefix(phrase, "roughly ")
	phrase = strings.TrimPrefix(phrase, "approximately ")
	if match := amountPattern.FindStringSubmatch(phrase); match != nil {
		value, err := strconv.ParseFloat(match[1], 64)
		if err != nil || math.IsInf(value, 0) || math.IsNaN(value) || math.Abs(value) > maxInput {
			return 0, "", false
		}
		unit := strings.TrimSpace(match[2])
		if _, ok := lookupUnit(unit); !ok {
			return 0, "", false
		}
		return value, unit, true
	}
	for _, article := range []string{"a ", "an "} {
		if strings.HasPrefix(phrase, article) {
			unit := strings.TrimSpace(phrase[len(article):])
			if _, ok := lookupUnit(unit); ok {
				return 1, unit, true
			}
		}
	}
	return 0, "", false
}

// extract fills the Tool from ordinary conversion phrasing. It returns false
// when either unit is unknown, so "how many calories in an apple" stays on the
// Agent path instead of becoming a Fast path guess.
func extract(text string) (In, bool) {
	raw := strings.ToLower(strings.TrimSpace(text))
	raw = strings.Trim(raw, " \t\r\n?!.,")
	raw = strings.ReplaceAll(raw, "-", " ")

	// "how many km in 5 miles" reverses the source and target.
	if rest, ok := cutPrefix(raw, "how many "); ok {
		rest = strings.TrimSpace(rest)
		rest = strings.Replace(rest, " are there in ", " in ", 1)
		rest = strings.Replace(rest, " are in ", " in ", 1)
		left, right, ok := strings.Cut(rest, " in ")
		if !ok {
			return In{}, false
		}
		target := strings.TrimSpace(left)
		if _, ok := lookupUnit(target); !ok {
			return In{}, false
		}
		value, from, ok := amountAndUnit(right)
		if !ok {
			return In{}, false
		}
		return In{Value: value, From: from, To: target}, true
	}

	for _, prefix := range []string{
		"convert ", "what is ", "what's ", "whats ", "what was ",
		"how much is ", "calculate ", "change ", "turn ",
	} {
		raw = strings.TrimPrefix(raw, prefix)
	}
	raw = strings.TrimSpace(raw)
	raw = strings.TrimSuffix(raw, " for me")
	raw = strings.TrimSuffix(raw, " please")
	raw = strings.TrimSpace(raw)

	// "in" is both the connector and the inch symbol, so the last connector
	// wins. "5 in in cm" and "5 miles in km" both split at the intended place.
	connector := ""
	index := -1
	for _, candidate := range []string{" to ", " into ", " as "} {
		if at := strings.Index(raw, candidate); at >= 0 {
			connector, index = candidate, at
			break
		}
	}
	if index < 0 {
		if at := strings.LastIndex(raw, " in "); at >= 0 {
			connector, index = " in ", at
		}
	}
	if index < 0 {
		return In{}, false
	}
	left := strings.TrimSpace(raw[:index])
	right := strings.TrimSpace(raw[index+len(connector):])
	value, from, ok := amountAndUnit(left)
	if !ok {
		return In{}, false
	}
	if _, ok := lookupUnit(right); !ok {
		return In{}, false
	}
	return In{Value: value, From: from, To: right}, true
}

// cutPrefix is strings.CutPrefix without the Go 1.20 result-documenting
// wrapper; it keeps the extraction condition obvious.
func cutPrefix(text, prefix string) (string, bool) {
	if !strings.HasPrefix(text, prefix) {
		return text, false
	}
	return text[len(prefix):], true
}

// formatNumber trims a float to a short form suitable for speech.
func formatNumber(value float64) string {
	if value == 0 {
		return "0"
	}
	if math.Abs(value) < 0.000001 {
		return strconv.FormatFloat(value, 'g', 6, 64)
	}
	text := strconv.FormatFloat(value, 'f', 6, 64)
	text = strings.TrimRight(text, "0")
	text = strings.TrimRight(text, ".")
	if text == "-0" {
		return "0"
	}
	return text
}

// Module is the units Tool domain. It has no settings.
type Module struct{}

// NewModule builds the units module.
func NewModule() *Module { return &Module{} }

// Name is the module name and config section suffix.
func (*Module) Name() string { return "units" }

// Register adds units.convert.
func (*Module) Register(r *tool.Registry, _ tool.Deps) error {
	return tool.Add(r, New())
}
