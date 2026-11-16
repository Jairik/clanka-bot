package units

// Currency table for units.currency. The ISO codes are the ECB currencies
// Frankfurter publishes; the spoken aliases are the forms users actually say.
// "dollars" (and "dollar") default to USD, documented as the chosen default for
// an ambiguous word; ambiguous words with no safe default (peso, franc on its
// own, real) are deliberately absent so they are refused rather than guessed.

import (
	"math"
	"strings"
)

// currencyDef is one ISO currency and its spoken singular/plural names.
type currencyDef struct {
	code     string
	singular string
	plural   string
}

var currencyDefs = map[string]currencyDef{
	"USD": {"USD", "US dollar", "US dollars"},
	"EUR": {"EUR", "euro", "euros"},
	"GBP": {"GBP", "pound", "pounds"},
	"JPY": {"JPY", "yen", "yen"},
	"CAD": {"CAD", "Canadian dollar", "Canadian dollars"},
	"AUD": {"AUD", "Australian dollar", "Australian dollars"},
	"CHF": {"CHF", "Swiss franc", "Swiss francs"},
	"CNY": {"CNY", "yuan", "yuan"},
	"INR": {"INR", "Indian rupee", "Indian rupees"},
	"MXN": {"MXN", "Mexican peso", "Mexican pesos"},
	"BRL": {"BRL", "Brazilian real", "Brazilian reais"},
	"NZD": {"NZD", "New Zealand dollar", "New Zealand dollars"},
	"SGD": {"SGD", "Singapore dollar", "Singapore dollars"},
	"HKD": {"HKD", "Hong Kong dollar", "Hong Kong dollars"},
	"SEK": {"SEK", "Swedish krona", "Swedish kronor"},
	"NOK": {"NOK", "Norwegian krone", "Norwegian kroner"},
	"DKK": {"DKK", "Danish krone", "Danish kroner"},
	"ZAR": {"ZAR", "South African rand", "South African rand"},
	"KRW": {"KRW", "South Korean won", "South Korean won"},
	"THB": {"THB", "Thai baht", "Thai baht"},
	"PLN": {"PLN", "Polish zloty", "Polish zloty"},
	"TRY": {"TRY", "Turkish lira", "Turkish lira"},
	"ILS": {"ILS", "Israeli shekel", "Israeli shekels"},
	"PHP": {"PHP", "Philippine peso", "Philippine pesos"},
	"IDR": {"IDR", "Indonesian rupiah", "Indonesian rupiah"},
	"CZK": {"CZK", "Czech koruna", "Czech koruna"},
	"HUF": {"HUF", "Hungarian forint", "Hungarian forint"},
	"RON": {"RON", "Romanian leu", "Romanian lei"},
	"BGN": {"BGN", "Bulgarian lev", "Bulgarian leva"},
	"ISK": {"ISK", "Icelandic krona", "Icelandic kronur"},
}

// currencyAliases maps spoken and common written forms to an ISO code. Only
// unambiguous forms are listed; ambiguous bare words (peso, franc, krona on its
// own, real) are omitted on purpose.
var currencyAliases = map[string]string{
	// US dollar is the explicit default for the bare word.
	"us dollar": "USD", "us dollars": "USD",
	"american dollar": "USD", "american dollars": "USD",
	"dollar": "USD", "dollars": "USD",
	"buck": "USD", "bucks": "USD",

	"euro": "EUR", "euros": "EUR",

	"pound": "GBP", "pounds": "GBP",
	"pound sterling": "GBP", "pounds sterling": "GBP",
	"british pound": "GBP", "british pounds": "GBP",
	"quid": "GBP",

	"yen": "JPY", "japanese yen": "JPY",

	"canadian dollar": "CAD", "canadian dollars": "CAD",
	"australian dollar": "AUD", "australian dollars": "AUD",
	"swiss franc": "CHF", "swiss francs": "CHF",

	"yuan": "CNY", "chinese yuan": "CNY", "renminbi": "CNY",

	"indian rupee": "INR", "indian rupees": "INR", "rupee": "INR", "rupees": "INR",
	"mexican peso": "MXN", "mexican pesos": "MXN",
	"brazilian real": "BRL", "brazilian reais": "BRL",
	"new zealand dollar": "NZD", "new zealand dollars": "NZD",
	"singapore dollar": "SGD", "singapore dollars": "SGD",
	"hong kong dollar": "HKD", "hong kong dollars": "HKD",

	"swedish krona": "SEK", "swedish kronor": "SEK",
	"norwegian krone": "NOK", "norwegian kroner": "NOK",
	"danish krone": "DKK", "danish kroner": "DKK",

	"rand": "ZAR", "south african rand": "ZAR",
	"won": "KRW", "south korean won": "KRW",
	"baht": "THB", "thai baht": "THB",
	"zloty": "PLN", "polish zloty": "PLN",
	"lira": "TRY", "turkish lira": "TRY",
	"shekel": "ILS", "israeli shekel": "ILS", "israeli shekels": "ILS",
	"philippine peso": "PHP", "philippine pesos": "PHP",
	"rupiah": "IDR", "indonesian rupiah": "IDR",
	"koruna": "CZK", "czech koruna": "CZK",
	"forint": "HUF", "hungarian forint": "HUF",
	"leu": "RON", "romanian leu": "RON", "romanian lei": "RON",
	"lev": "BGN", "bulgarian lev": "BGN", "bulgarian leva": "BGN",
	"krona": "ISK", "icelandic krona": "ISK", "icelandic kronur": "ISK",
}

// resolveCurrency turns a spoken or ISO currency name into its ISO code.
func resolveCurrency(name string) (string, bool) {
	normalized := normalizeCurrency(name)
	if iso, ok := currencyAliases[normalized]; ok {
		return iso, true
	}
	upper := strings.ToUpper(strings.TrimSpace(normalized))
	if _, ok := currencyDefs[upper]; ok {
		return upper, true
	}
	return "", false
}

// normalizeCurrency lowercases, drops periods and collapses spaces.
func normalizeCurrency(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	name = strings.ReplaceAll(name, ".", "")
	return strings.Join(strings.Fields(name), " ")
}

// currencyNoun picks the singular or plural spoken name for a value.
func currencyNoun(iso string, value float64) string {
	def, ok := currencyDefs[iso]
	if !ok {
		return iso
	}
	if math.Abs(value) == 1 {
		return def.singular
	}
	return def.plural
}
