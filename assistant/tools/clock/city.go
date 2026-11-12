package clock

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// CityName is the canonical time.in_city Tool name.
const CityName = "time.in_city"

// city is one shipped city-to-IANA entry. The table is deliberately local: no
// geocoding, no IP lookup and no network call is made to answer the time.
type city struct {
	Name string
	Zone string
}

// cities maps normalised spoken names to their timezone. Coverage is bounded:
// 278 named cities with 324 spoken aliases across 149 IANA zones on six
// continents. A name that is missing returns a truthful unknown-city reply
// instead of guessing; bare ambiguous names are refused.
var cities = map[string]city{
	// Japan and Korea.
	"tokyo":    {"Tokyo", "Asia/Tokyo"},
	"osaka":    {"Osaka", "Asia/Tokyo"},
	"kyoto":    {"Kyoto", "Asia/Tokyo"},
	"yokohama": {"Yokohama", "Asia/Tokyo"},
	"nagoya":   {"Nagoya", "Asia/Tokyo"},
	"sapporo":  {"Sapporo", "Asia/Tokyo"},
	"fukuoka":  {"Fukuoka", "Asia/Tokyo"},
	"kobe":     {"Kobe", "Asia/Tokyo"},
	"seoul":    {"Seoul", "Asia/Seoul"},
	"busan":    {"Busan", "Asia/Seoul"},
	"incheon":  {"Incheon", "Asia/Seoul"},

	// China, Hong Kong, Macau and Taiwan.
	"beijing":   {"Beijing", "Asia/Shanghai"},
	"peking":    {"Beijing", "Asia/Shanghai"},
	"shanghai":  {"Shanghai", "Asia/Shanghai"},
	"shenzhen":  {"Shenzhen", "Asia/Shanghai"},
	"guangzhou": {"Guangzhou", "Asia/Shanghai"},
	"chengdu":   {"Chengdu", "Asia/Shanghai"},
	"hangzhou":  {"Hangzhou", "Asia/Shanghai"},
	"wuhan":     {"Wuhan", "Asia/Shanghai"},
	"xian":      {"Xi'an", "Asia/Shanghai"},
	"hong kong": {"Hong Kong", "Asia/Hong_Kong"},
	"macau":     {"Macau", "Asia/Macau"},
	"taipei":    {"Taipei", "Asia/Taipei"},
	"kaohsiung": {"Kaohsiung", "Asia/Taipei"},

	// South-east and South Asia.
	"singapore":        {"Singapore", "Asia/Singapore"},
	"bangkok":          {"Bangkok", "Asia/Bangkok"},
	"jakarta":          {"Jakarta", "Asia/Jakarta"},
	"denpasar":         {"Denpasar", "Asia/Makassar"},
	"bali":             {"Bali", "Asia/Makassar"},
	"manila":           {"Manila", "Asia/Manila"},
	"kuala lumpur":     {"Kuala Lumpur", "Asia/Kuala_Lumpur"},
	"hanoi":            {"Hanoi", "Asia/Ho_Chi_Minh"},
	"ho chi minh city": {"Ho Chi Minh City", "Asia/Ho_Chi_Minh"},
	"saigon":           {"Ho Chi Minh City", "Asia/Ho_Chi_Minh"},
	"phnom penh":       {"Phnom Penh", "Asia/Phnom_Penh"},
	"vientiane":        {"Vientiane", "Asia/Vientiane"},
	"yangon":           {"Yangon", "Asia/Yangon"},
	"delhi":            {"Delhi", "Asia/Kolkata"},
	"new delhi":        {"New Delhi", "Asia/Kolkata"},
	"mumbai":           {"Mumbai", "Asia/Kolkata"},
	"bombay":           {"Mumbai", "Asia/Kolkata"},
	"bangalore":        {"Bangalore", "Asia/Kolkata"},
	"bengaluru":        {"Bangalore", "Asia/Kolkata"},
	"kolkata":          {"Kolkata", "Asia/Kolkata"},
	"calcutta":         {"Kolkata", "Asia/Kolkata"},
	"chennai":          {"Chennai", "Asia/Kolkata"},
	"madras":           {"Chennai", "Asia/Kolkata"},
	"hyderabad":        {"Hyderabad", "Asia/Kolkata"},
	"ahmedabad":        {"Ahmedabad", "Asia/Kolkata"},
	"pune":             {"Pune", "Asia/Kolkata"},
	"karachi":          {"Karachi", "Asia/Karachi"},
	"lahore":           {"Lahore", "Asia/Karachi"},
	"islamabad":        {"Islamabad", "Asia/Karachi"},
	"dhaka":            {"Dhaka", "Asia/Dhaka"},
	"kathmandu":        {"Kathmandu", "Asia/Kathmandu"},
	"colombo":          {"Colombo", "Asia/Colombo"},
	"male":             {"Male", "Indian/Maldives"},

	// Middle East.
	"dubai":       {"Dubai", "Asia/Dubai"},
	"abu dhabi":   {"Abu Dhabi", "Asia/Dubai"},
	"doha":        {"Doha", "Asia/Qatar"},
	"riyadh":      {"Riyadh", "Asia/Riyadh"},
	"jeddah":      {"Jeddah", "Asia/Riyadh"},
	"mecca":       {"Mecca", "Asia/Riyadh"},
	"tehran":      {"Tehran", "Asia/Tehran"},
	"baghdad":     {"Baghdad", "Asia/Baghdad"},
	"tel aviv":    {"Tel Aviv", "Asia/Jerusalem"},
	"jerusalem":   {"Jerusalem", "Asia/Jerusalem"},
	"beirut":      {"Beirut", "Asia/Beirut"},
	"amman":       {"Amman", "Asia/Amman"},
	"kuwait city": {"Kuwait City", "Asia/Kuwait"},
	"muscat":      {"Muscat", "Asia/Muscat"},
	"istanbul":    {"Istanbul", "Europe/Istanbul"},
	"ankara":      {"Ankara", "Europe/Istanbul"},
	"izmir":       {"Izmir", "Europe/Istanbul"},

	// Russia, Caucasus and Central Asia.
	"moscow":           {"Moscow", "Europe/Moscow"},
	"saint petersburg": {"Saint Petersburg", "Europe/Moscow"},
	"st petersburg":    {"Saint Petersburg", "Europe/Moscow"},
	"kyiv":             {"Kyiv", "Europe/Kyiv"},
	"kiev":             {"Kyiv", "Europe/Kyiv"},
	"minsk":            {"Minsk", "Europe/Minsk"},
	"tbilisi":          {"Tbilisi", "Asia/Tbilisi"},
	"yerevan":          {"Yerevan", "Asia/Yerevan"},
	"baku":             {"Baku", "Asia/Baku"},
	"almaty":           {"Almaty", "Asia/Almaty"},
	"tashkent":         {"Tashkent", "Asia/Tashkent"},
	"astana":           {"Astana", "Asia/Almaty"},
	"ulaanbaatar":      {"Ulaanbaatar", "Asia/Ulaanbaatar"},

	// Western and Northern Europe.
	"london":     {"London", "Europe/London"},
	"manchester": {"Manchester", "Europe/London"},
	"liverpool":  {"Liverpool", "Europe/London"},
	"edinburgh":  {"Edinburgh", "Europe/London"},
	"glasgow":    {"Glasgow", "Europe/London"},
	"belfast":    {"Belfast", "Europe/London"},
	"cardiff":    {"Cardiff", "Europe/London"},
	"dublin":     {"Dublin", "Europe/Dublin"},
	"paris":      {"Paris", "Europe/Paris"},
	"lyon":       {"Lyon", "Europe/Paris"},
	"marseille":  {"Marseille", "Europe/Paris"},
	"toulouse":   {"Toulouse", "Europe/Paris"},
	"nice":       {"Nice", "Europe/Paris"},
	"berlin":     {"Berlin", "Europe/Berlin"},
	"munich":     {"Munich", "Europe/Berlin"},
	"muenchen":   {"Munich", "Europe/Berlin"},
	"hamburg":    {"Hamburg", "Europe/Berlin"},
	"frankfurt":  {"Frankfurt", "Europe/Berlin"},
	"cologne":    {"Cologne", "Europe/Berlin"},
	"koeln":      {"Cologne", "Europe/Berlin"},
	"stuttgart":  {"Stuttgart", "Europe/Berlin"},
	"dusseldorf": {"Dusseldorf", "Europe/Berlin"},
	"düsseldorf": {"Dusseldorf", "Europe/Berlin"},
	"amsterdam":  {"Amsterdam", "Europe/Amsterdam"},
	"rotterdam":  {"Rotterdam", "Europe/Amsterdam"},
	"the hague":  {"The Hague", "Europe/Amsterdam"},
	"hague":      {"The Hague", "Europe/Amsterdam"},
	"brussels":   {"Brussels", "Europe/Brussels"},
	"antwerp":    {"Antwerp", "Europe/Brussels"},
	"zurich":     {"Zurich", "Europe/Zurich"},
	"zürich":     {"Zurich", "Europe/Zurich"},
	"geneva":     {"Geneva", "Europe/Zurich"},
	"basel":      {"Basel", "Europe/Zurich"},
	"bern":       {"Bern", "Europe/Zurich"},
	"vienna":     {"Vienna", "Europe/Vienna"},
	"wien":       {"Vienna", "Europe/Vienna"},
	"salzburg":   {"Salzburg", "Europe/Vienna"},
	"prague":     {"Prague", "Europe/Prague"},
	"praha":      {"Prague", "Europe/Prague"},
	"bratislava": {"Bratislava", "Europe/Bratislava"},
	"budapest":   {"Budapest", "Europe/Budapest"},
	"warsaw":     {"Warsaw", "Europe/Warsaw"},
	"warszawa":   {"Warsaw", "Europe/Warsaw"},
	"krakow":     {"Krakow", "Europe/Warsaw"},
	"gdansk":     {"Gdansk", "Europe/Warsaw"},
	"vilnius":    {"Vilnius", "Europe/Vilnius"},
	"riga":       {"Riga", "Europe/Riga"},
	"tallinn":    {"Tallinn", "Europe/Tallinn"},
	"helsinki":   {"Helsinki", "Europe/Helsinki"},
	"stockholm":  {"Stockholm", "Europe/Stockholm"},
	"gothenburg": {"Gothenburg", "Europe/Stockholm"},
	"goteborg":   {"Gothenburg", "Europe/Stockholm"},
	"oslo":       {"Oslo", "Europe/Oslo"},
	"bergen":     {"Bergen", "Europe/Oslo"},
	"copenhagen": {"Copenhagen", "Europe/Copenhagen"},
	"reykjavik":  {"Reykjavik", "Atlantic/Reykjavik"},

	// Southern and Eastern Europe.
	"lisbon":       {"Lisbon", "Europe/Lisbon"},
	"lisboa":       {"Lisbon", "Europe/Lisbon"},
	"porto":        {"Porto", "Europe/Lisbon"},
	"madrid":       {"Madrid", "Europe/Madrid"},
	"barcelona":    {"Barcelona", "Europe/Madrid"},
	"seville":      {"Seville", "Europe/Madrid"},
	"valencia":     {"Valencia", "Europe/Madrid"},
	"rome":         {"Rome", "Europe/Rome"},
	"roma":         {"Rome", "Europe/Rome"},
	"milan":        {"Milan", "Europe/Rome"},
	"milano":       {"Milan", "Europe/Rome"},
	"naples":       {"Naples", "Europe/Rome"},
	"turin":        {"Turin", "Europe/Rome"},
	"florence":     {"Florence", "Europe/Rome"},
	"venice":       {"Venice", "Europe/Rome"},
	"athens":       {"Athens", "Europe/Athens"},
	"thessaloniki": {"Thessaloniki", "Europe/Athens"},
	"sofia":        {"Sofia", "Europe/Sofia"},
	"bucharest":    {"Bucharest", "Europe/Bucharest"},
	"belgrade":     {"Belgrade", "Europe/Belgrade"},
	"zagreb":       {"Zagreb", "Europe/Zagreb"},
	"sarajevo":     {"Sarajevo", "Europe/Sarajevo"},
	"skopje":       {"Skopje", "Europe/Skopje"},
	"tirana":       {"Tirana", "Europe/Tirane"},
	"podgorica":    {"Podgorica", "Europe/Podgorica"},
	"ljubljana":    {"Ljubljana", "Europe/Ljubljana"},
	"valletta":     {"Valletta", "Europe/Malta"},
	"luxembourg":   {"Luxembourg", "Europe/Luxembourg"},
	"monaco":       {"Monaco", "Europe/Monaco"},
	"andorra":      {"Andorra", "Europe/Andorra"},

	// United States and Canada.
	"new york":        {"New York", "America/New_York"},
	"new york city":   {"New York", "America/New_York"},
	"nyc":             {"New York", "America/New_York"},
	"boston":          {"Boston", "America/New_York"},
	"philadelphia":    {"Philadelphia", "America/New_York"},
	"washington":      {"Washington", "America/New_York"},
	"washington dc":   {"Washington", "America/New_York"},
	"dc":              {"Washington", "America/New_York"},
	"miami":           {"Miami", "America/New_York"},
	"atlanta":         {"Atlanta", "America/New_York"},
	"detroit":         {"Detroit", "America/New_York"},
	"baltimore":       {"Baltimore", "America/New_York"},
	"pittsburgh":      {"Pittsburgh", "America/New_York"},
	"cleveland":       {"Cleveland", "America/New_York"},
	"charlotte":       {"Charlotte", "America/New_York"},
	"orlando":         {"Orlando", "America/New_York"},
	"tampa":           {"Tampa", "America/New_York"},
	"toronto":         {"Toronto", "America/Toronto"},
	"montreal":        {"Montreal", "America/Toronto"},
	"ottawa":          {"Ottawa", "America/Toronto"},
	"quebec city":     {"Quebec City", "America/Toronto"},
	"chicago":         {"Chicago", "America/Chicago"},
	"houston":         {"Houston", "America/Chicago"},
	"dallas":          {"Dallas", "America/Chicago"},
	"austin":          {"Austin", "America/Chicago"},
	"san antonio":     {"San Antonio", "America/Chicago"},
	"minneapolis":     {"Minneapolis", "America/Chicago"},
	"st louis":        {"St. Louis", "America/Chicago"},
	"saint louis":     {"St. Louis", "America/Chicago"},
	"new orleans":     {"New Orleans", "America/Chicago"},
	"nashville":       {"Nashville", "America/Chicago"},
	"memphis":         {"Memphis", "America/Chicago"},
	"milwaukee":       {"Milwaukee", "America/Chicago"},
	"kansas city":     {"Kansas City", "America/Chicago"},
	"oklahoma city":   {"Oklahoma City", "America/Chicago"},
	"denver":          {"Denver", "America/Denver"},
	"salt lake city":  {"Salt Lake City", "America/Denver"},
	"albuquerque":     {"Albuquerque", "America/Denver"},
	"phoenix":         {"Phoenix", "America/Phoenix"},
	"tucson":          {"Tucson", "America/Phoenix"},
	"los angeles":     {"Los Angeles", "America/Los_Angeles"},
	"la":              {"Los Angeles", "America/Los_Angeles"},
	"san francisco":   {"San Francisco", "America/Los_Angeles"},
	"sf":              {"San Francisco", "America/Los_Angeles"},
	"seattle":         {"Seattle", "America/Los_Angeles"},
	"san diego":       {"San Diego", "America/Los_Angeles"},
	"las vegas":       {"Las Vegas", "America/Los_Angeles"},
	"sacramento":      {"Sacramento", "America/Los_Angeles"},
	"san jose":        {"San Jose", "America/Los_Angeles"},
	"oakland":         {"Oakland", "America/Los_Angeles"},
	"portland oregon": {"Portland", "America/Los_Angeles"},
	"portland or":     {"Portland", "America/Los_Angeles"},
	"portland maine":  {"Portland", "America/New_York"},
	"portland me":     {"Portland", "America/New_York"},
	"vancouver":       {"Vancouver", "America/Vancouver"},
	"calgary":         {"Calgary", "America/Edmonton"},
	"edmonton":        {"Edmonton", "America/Edmonton"},
	"winnipeg":        {"Winnipeg", "America/Winnipeg"},
	"halifax":         {"Halifax", "America/Halifax"},
	"st johns":        {"St. John's", "America/St_Johns"},
	"saint johns":     {"St. John's", "America/St_Johns"},
	"anchorage":       {"Anchorage", "America/Anchorage"},
	"honolulu":        {"Honolulu", "Pacific/Honolulu"},

	// Mexico, Central America and the Caribbean.
	"mexico city":         {"Mexico City", "America/Mexico_City"},
	"guadalajara":         {"Guadalajara", "America/Mexico_City"},
	"monterrey":           {"Monterrey", "America/Monterrey"},
	"cancun":              {"Cancun", "America/Cancun"},
	"tijuana":             {"Tijuana", "America/Tijuana"},
	"havana":              {"Havana", "America/Havana"},
	"san juan":            {"San Juan", "America/Puerto_Rico"},
	"kingston":            {"Kingston", "America/Jamaica"},
	"panama city":         {"Panama City", "America/Panama"},
	"san jose costa rica": {"San Jose", "America/Costa_Rica"},
	"guatemala city":      {"Guatemala City", "America/Guatemala"},

	// South America.
	"lima":           {"Lima", "America/Lima"},
	"bogota":         {"Bogota", "America/Bogota"},
	"bogotá":         {"Bogota", "America/Bogota"},
	"quito":          {"Quito", "America/Guayaquil"},
	"caracas":        {"Caracas", "America/Caracas"},
	"la paz":         {"La Paz", "America/La_Paz"},
	"santiago":       {"Santiago", "America/Santiago"},
	"buenos aires":   {"Buenos Aires", "America/Argentina/Buenos_Aires"},
	"montevideo":     {"Montevideo", "America/Montevideo"},
	"asuncion":       {"Asuncion", "America/Asuncion"},
	"sao paulo":      {"Sao Paulo", "America/Sao_Paulo"},
	"são paulo":      {"Sao Paulo", "America/Sao_Paulo"},
	"rio de janeiro": {"Rio de Janeiro", "America/Sao_Paulo"},
	"brasilia":       {"Brasilia", "America/Sao_Paulo"},
	"brasília":       {"Brasilia", "America/Sao_Paulo"},
	"recife":         {"Recife", "America/Recife"},
	"fortaleza":      {"Fortaleza", "America/Fortaleza"},
	"porto alegre":   {"Porto Alegre", "America/Sao_Paulo"},
	"paramaribo":     {"Paramaribo", "America/Paramaribo"},

	// Africa.
	"cairo":         {"Cairo", "Africa/Cairo"},
	"alexandria":    {"Alexandria", "Africa/Cairo"},
	"lagos":         {"Lagos", "Africa/Lagos"},
	"abuja":         {"Abuja", "Africa/Lagos"},
	"accra":         {"Accra", "Africa/Accra"},
	"nairobi":       {"Nairobi", "Africa/Nairobi"},
	"addis ababa":   {"Addis Ababa", "Africa/Addis_Ababa"},
	"casablanca":    {"Casablanca", "Africa/Casablanca"},
	"rabat":         {"Rabat", "Africa/Casablanca"},
	"tunis":         {"Tunis", "Africa/Tunis"},
	"algiers":       {"Algiers", "Africa/Algiers"},
	"dakar":         {"Dakar", "Africa/Dakar"},
	"johannesburg":  {"Johannesburg", "Africa/Johannesburg"},
	"cape town":     {"Cape Town", "Africa/Johannesburg"},
	"durban":        {"Durban", "Africa/Johannesburg"},
	"pretoria":      {"Pretoria", "Africa/Johannesburg"},
	"kinshasa":      {"Kinshasa", "Africa/Kinshasa"},
	"luanda":        {"Luanda", "Africa/Luanda"},
	"dar es salaam": {"Dar es Salaam", "Africa/Dar_es_Salaam"},
	"kampala":       {"Kampala", "Africa/Kampala"},
	"khartoum":      {"Khartoum", "Africa/Khartoum"},
	"kigali":        {"Kigali", "Africa/Kigali"},
	"maputo":        {"Maputo", "Africa/Maputo"},
	"windhoek":      {"Windhoek", "Africa/Windhoek"},
	"harare":        {"Harare", "Africa/Harare"},
	"lusaka":        {"Lusaka", "Africa/Lusaka"},
	"gaborone":      {"Gaborone", "Africa/Gaborone"},
	"antananarivo":  {"Antananarivo", "Indian/Antananarivo"},

	// Oceania.
	"sydney":       {"Sydney", "Australia/Sydney"},
	"canberra":     {"Canberra", "Australia/Sydney"},
	"melbourne":    {"Melbourne", "Australia/Melbourne"},
	"brisbane":     {"Brisbane", "Australia/Brisbane"},
	"perth":        {"Perth", "Australia/Perth"},
	"adelaide":     {"Adelaide", "Australia/Adelaide"},
	"hobart":       {"Hobart", "Australia/Hobart"},
	"darwin":       {"Darwin", "Australia/Darwin"},
	"auckland":     {"Auckland", "Pacific/Auckland"},
	"wellington":   {"Wellington", "Pacific/Auckland"},
	"christchurch": {"Christchurch", "Pacific/Auckland"},
	"suva":         {"Suva", "Pacific/Fiji"},
	"fiji":         {"Suva", "Pacific/Fiji"},
	"port moresby": {"Port Moresby", "Pacific/Port_Moresby"},
	"noumea":       {"Noumea", "Pacific/Noumea"},
	"papeete":      {"Papeete", "Pacific/Tahiti"},

	// Qualified names for common ambiguous cities.
	"birmingham uk":             {"Birmingham", "Europe/London"},
	"birmingham england":        {"Birmingham", "Europe/London"},
	"birmingham alabama":        {"Birmingham", "America/Chicago"},
	"birmingham al":             {"Birmingham", "America/Chicago"},
	"springfield illinois":      {"Springfield", "America/Chicago"},
	"springfield il":            {"Springfield", "America/Chicago"},
	"springfield massachusetts": {"Springfield", "America/New_York"},
	"springfield ma":            {"Springfield", "America/New_York"},
	"cambridge uk":              {"Cambridge", "Europe/London"},
	"cambridge england":         {"Cambridge", "Europe/London"},
	"cambridge massachusetts":   {"Cambridge", "America/New_York"},
	"cambridge ma":              {"Cambridge", "America/New_York"},
	"columbus ohio":             {"Columbus", "America/New_York"},
	"columbus oh":               {"Columbus", "America/New_York"},
}

// ambiguousCities refuses a bare name that maps to more than one zone. The
// value is a hint naming qualified forms the user can say instead.
var ambiguousCities = map[string]string{
	"portland":    `Portland, Oregon or Portland, Maine`,
	"birmingham":  `Birmingham, UK or Birmingham, Alabama`,
	"springfield": `Springfield, Illinois or Springfield, Massachusetts`,
	"cambridge":   `Cambridge, UK or Cambridge, Massachusetts`,
	"columbus":    `Columbus, Ohio`,
}

// CityIn names a city.
type CityIn struct {
	City string `json:"city"`
}

// CityOut is the current time in that city's own zone.
type CityOut struct {
	City     string    `json:"city"`
	Timezone string    `json:"timezone"`
	Time     time.Time `json:"time"`
}

// NewInCity builds time.in_city. A nil now uses time.Now.
func NewInCity(now func() time.Time) tool.Tool[CityIn, CityOut] {
	if now == nil {
		now = time.Now
	}
	return tool.Tool[CityIn, CityOut]{
		Name:        CityName,
		Description: "Get the current time in a named city, using a local timezone table and no network lookup.",
		Permission:  tool.PermissionRead,
		LocalOnly:   tool.LocalOnlyNo,
		Handler: func(_ context.Context, in CityIn) (CityOut, error) {
			entry, ok := findCity(in.City)
			if !ok {
				return CityOut{}, cityError(in.City)
			}
			location, err := time.LoadLocation(entry.Zone)
			if err != nil {
				return CityOut{}, &tool.ToolError{Say: fmt.Sprintf("I don't have timezone data for %s.", entry.Name)}
			}
			return CityOut{City: entry.Name, Timezone: entry.Zone, Time: now().In(location)}, nil
		},
		Speak: func(out CityOut) string {
			return "It's " + out.Time.Format("3:04 PM") + " in " + out.City + "."
		},
		Extract: extractCity,
		Examples: []string{
			"what time is it in Tokyo",
			"what's the time in New York",
			"what time is it in London",
			"tell me the time in Paris",
			"current time in Sydney",
			"what time is it in Los Angeles",
			"what's the local time in Berlin",
			"time in Tokyo",
		},
	}
}

// RegisterCity adds time.in_city to the registry.
func RegisterCity(r *tool.Registry, now func() time.Time) error {
	return tool.Add(r, NewInCity(now))
}

// findCity resolves a spoken city name, or reports that it is unknown or
// ambiguous. The second result is false in both failure cases so Extract keeps
// the request on the Agent path.
func findCity(raw string) (city, bool) {
	for _, key := range cityKeys(raw) {
		if entry, ok := cities[key]; ok {
			return entry, true
		}
		// "Tokyo city" and similar filler should still reach Tokyo. The table
		// is checked first so an explicit "Mexico City" entry wins.
		if strings.HasSuffix(key, " city") {
			if entry, ok := cities[strings.TrimSuffix(key, " city")]; ok {
				return entry, true
			}
		}
	}
	return city{}, false
}

// cityError distinguishes an ambiguous city (with a hint) from an unknown one.
func cityError(raw string) error {
	for _, key := range cityKeys(raw) {
		if hint, ok := ambiguousCities[key]; ok {
			return &tool.ToolError{Say: fmt.Sprintf("I know more than one city called %q. Try %s.", strings.TrimSpace(raw), hint)}
		}
	}
	return &tool.ToolError{Say: fmt.Sprintf("I don't know the timezone for %q yet. I know major cities such as Tokyo, London, New York and Sydney.", strings.TrimSpace(raw))}
}

// cityKeys returns the full normalised name first, then the same name without
// a trailing country, then the part before a comma. "Portland, Oregon"
// therefore matches its qualified entry, while "Tokyo, Japan" falls back to
// Tokyo.
func cityKeys(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	keys := []string{normalizeCity(raw)}
	fields := strings.Fields(keys[0])
	for len(fields) > 1 {
		last := fields[len(fields)-1]
		if !placeSuffixes[last] {
			break
		}
		fields = fields[:len(fields)-1]
		keys = append(keys, strings.Join(fields, " "))
	}
	if comma := strings.Index(raw, ","); comma >= 0 {
		if base := normalizeCity(raw[:comma]); base != "" && !containsKey(keys, base) {
			keys = append(keys, base)
		}
	}
	return keys
}

// placeSuffixes is the bounded set of country words a spoken city may trail.
// A state or province suffix is deliberately not stripped: "Washington state"
// is not the same request as "Washington".
var placeSuffixes = map[string]bool{
	"japan": true, "france": true, "england": true, "scotland": true, "wales": true,
	"ireland": true, "uk": true, "germany": true, "italy": true, "spain": true,
	"portugal": true, "netherlands": true, "belgium": true, "switzerland": true,
	"austria": true, "poland": true, "czechia": true, "sweden": true, "norway": true,
	"denmark": true, "finland": true, "iceland": true, "greece": true, "turkey": true,
	"russia": true, "ukraine": true, "canada": true, "usa": true, "us": true,
	"america": true, "mexico": true, "brazil": true, "argentina": true, "chile": true,
	"peru": true, "colombia": true, "australia": true, "china": true, "india": true,
	"korea": true, "thailand": true, "vietnam": true, "indonesia": true,
	"philippines": true, "malaysia": true, "egypt": true, "kenya": true,
	"nigeria": true, "morocco": true, "uae": true, "israel": true, "qatar": true,
}

// containsKey is a small membership check for candidate keys.
func containsKey(keys []string, want string) bool {
	for _, key := range keys {
		if key == want {
			return true
		}
	}
	return false
}

// normalizeCity lowercases a name and keeps letters, digits and single spaces.
// Punctuation is a separator rather than part of the key, except apostrophes,
// which are dropped so "St. John's" becomes "st johns" and "Xi'an" becomes
// "xian".
func normalizeCity(raw string) string {
	name := strings.ToLower(strings.TrimSpace(raw))
	name = strings.TrimPrefix(name, "the ")
	var builder strings.Builder
	lastSpace := false
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r > 127:
			builder.WriteRune(r)
			lastSpace = false
		case r == '\'' || r == '’':
			// Drop apostrophes without introducing a word break.
		case r == ' ' || r == '.' || r == '-' || r == ',':
			if !lastSpace && builder.Len() > 0 {
				builder.WriteByte(' ')
				lastSpace = true
			}
		}
	}
	return strings.TrimSpace(builder.String())
}

// extractCity takes the city after the last " in " and only succeeds when the
// table knows it. A timezone question such as "what time zone is Tokyo in"
// therefore stays on the Agent path.
func extractCity(text string) (CityIn, bool) {
	lower := strings.ToLower(strings.TrimSpace(text))
	// A time question is required: "how many people live in Tokyo" names a
	// city but is not a clock request.
	if !strings.Contains(lower, "time") && !strings.Contains(lower, "clock") {
		return CityIn{}, false
	}
	index := strings.LastIndex(lower, " in ")
	if index < 0 {
		return CityIn{}, false
	}
	name := strings.TrimSpace(text[index+len(" in "):])
	name = strings.Trim(name, " ?.!,;:")
	if name == "" {
		return CityIn{}, false
	}
	if _, ok := findCity(name); !ok {
		return CityIn{}, false
	}
	return CityIn{City: name}, true
}
