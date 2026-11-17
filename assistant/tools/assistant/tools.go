package assistant

import (
	"context"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/Jairik/clanka-bot/assistant/internal/announce"
	"github.com/Jairik/clanka-bot/assistant/internal/lastreply"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/internal/wyoming"
)

// RepeatIn carries an optional speech-rate direction. "say that again" has no
// direction and repeats at the setting; "say that again slower" steps down.
type RepeatIn struct {
	Rate string `json:"rate,omitempty"`
}

// Rate directions Extract recognises. An empty string keeps the setting.
const (
	RepeatSlower = "slower"
	RepeatFaster = "faster"
	RepeatHalf   = "half"
	RepeatDouble = "double"
)

// repeatStep is the fraction of the previous rate one "slower"/"faster" step
// changes. 20% keeps the change audible without overshooting the setting.
const repeatStep = 0.20

// RepeatOut is the previous completed reply with its provenance. Rate, when
// non-zero, is the per-utterance TTS multiplier to speak this replay at; it is
// not a change to the persistent voice.rate setting.
type RepeatOut struct {
	Text      string    `json:"text"`
	Tool      string    `json:"tool,omitempty"`
	Path      string    `json:"path,omitempty"`
	LocalOnly bool      `json:"local_only,omitempty"`
	At        time.Time `json:"at,omitempty"`
	Rate      float64   `json:"rate,omitempty"`
}

// HeardIn has no fields: reading the transcript the Assistant acted on takes
// no arguments.
type HeardIn struct{}

// HeardOut is the transcript the Assistant acted on with its provenance.
type HeardOut struct {
	Text      string    `json:"text"`
	Utterance string    `json:"utterance"`
	At        time.Time `json:"at,omitempty"`
	Source    string    `json:"source,omitempty"`
}

// ForgetIn has no fields: "forget the conversation" clears whatever is stored,
// it does not take the current utterance as its input.
type ForgetIn struct{}

// ForgetOut is the one-line confirmation that the history was cleared.
type ForgetOut struct {
	Text string `json:"text"`
}

// NewForget builds assistant.forget. It resets the shared conversation history
// on the Pi, so it works with no provider reachable. It is deliberately
// Local-only and Fast-path eligible (PermissionAct, not AgentOnly).
//
// It clears only conversation turns, never the last-reply store: "say that
// again" must still work after a forget. It does not clear Scheduled alerts,
// notes or lists; this is conversation state only. A nil history is a clear
// failure rather than a panic, for callers with no shared history wired.
func NewForget(history tool.HistoryResetter) tool.Tool[ForgetIn, ForgetOut] {
	return tool.Tool[ForgetIn, ForgetOut]{
		Name:        ForgetName,
		Description: "Forget the current conversation and start with a clean history.",
		Permission:  tool.PermissionAct,
		// A forget must work even when the provider is unreachable, and its
		// confirmation never leaves the Pi. LocalOnlyYes + AgentOnly is
		// rejected at registration, so this stays Fast-path eligible.
		LocalOnly: tool.LocalOnlyYes,
		Handler: func(context.Context, ForgetIn) (ForgetOut, error) {
			if history == nil {
				return ForgetOut{}, &tool.ToolError{Say: "I can't forget anything right now."}
			}
			history.Reset()
			return ForgetOut{Text: "Forgotten."}, nil
		},
		Speak: func(out ForgetOut) string { return out.Text },
		Examples: []string{
			"forget that",
			"forget the conversation",
			"clear our chat",
			"start over",
			"never mind that",
			"forget everything we've talked about",
			"let's start fresh",
			"clear the conversation history",
		},
	}
}

// AnnouncementQueue is the queue seam assistant.announcements reads.
type AnnouncementQueue interface {
	Recent() []announce.Line
}

// AnnouncementsIn has no fields: reading announcements takes no arguments.
type AnnouncementsIn struct{}

// AnnouncementsOut is the list of recent announcement lines and the one-line spoken form.
type AnnouncementsOut struct {
	Lines []announce.Line `json:"lines"`
	Text  string          `json:"text"`
}

// NewAnnouncements builds assistant.announcements. It is Local-only and read-only,
// reading lines from the in-memory queue ring.
func NewAnnouncements(queue AnnouncementQueue, nowSource ...func() time.Time) tool.Tool[AnnouncementsIn, AnnouncementsOut] {
	var nowFn func() time.Time
	if len(nowSource) > 0 && nowSource[0] != nil {
		nowFn = nowSource[0]
	} else {
		nowFn = time.Now
	}
	return tool.Tool[AnnouncementsIn, AnnouncementsOut]{
		Name:        AnnouncementsName,
		Description: "Read back recent announcements spoken while away.",
		Permission:  tool.PermissionRead,
		LocalOnly:   tool.LocalOnlyYes,
		Handler: func(context.Context, AnnouncementsIn) (AnnouncementsOut, error) {
			if queue == nil || (reflect.ValueOf(queue).Kind() == reflect.Pointer && reflect.ValueOf(queue).IsNil()) {
				return AnnouncementsOut{}, &tool.ToolError{Say: "I can't check that right now."}
			}
			lines := queue.Recent()
			text := formatAnnouncements(lines, nowFn())
			return AnnouncementsOut{Lines: lines, Text: text}, nil
		},
		Speak: func(out AnnouncementsOut) string { return out.Text },
		Examples: []string{
			"what did you say while I was out",
			"read me the announcements",
			"any announcements",
			"did you say anything while I was out",
		},
	}
}

// formatAnnouncements renders up to five announcements oldest-first with relative
// time, appending "and N more" when there are more than five lines.
func formatAnnouncements(lines []announce.Line, now time.Time) string {
	if len(lines) == 0 {
		return "Nothing."
	}
	maxLines := 5
	count := len(lines)
	if count > maxLines {
		count = maxLines
	}
	rendered := make([]string, 0, count)
	for i := 0; i < count; i++ {
		line := lines[i]
		rel := relativeTime(now, line.EnqueuedAt)
		text := strings.TrimSpace(line.Text)
		if text != "" && !strings.HasSuffix(text, ".") && !strings.HasSuffix(text, "!") && !strings.HasSuffix(text, "?") {
			text += "."
		}
		rendered = append(rendered, rel+": "+text)
	}
	if len(lines) > maxLines {
		remaining := len(lines) - maxLines
		last := strings.TrimSuffix(rendered[maxLines-1], ".")
		rendered[maxLines-1] = fmt.Sprintf("%s, and %d more.", last, remaining)
	}
	return strings.Join(rendered, " ")
}

// relativeTime renders a humanized relative time string for an enqueue instant.
func relativeTime(now, at time.Time) string {
	if at.IsZero() || at.After(now) {
		return "Just now"
	}
	d := now.Sub(at)
	if d < time.Minute {
		return "Just now"
	}
	if d < time.Hour {
		mins := int(d.Round(time.Minute) / time.Minute)
		if mins <= 1 {
			return "One minute ago"
		}
		return upperFirst(numberWord(mins)) + " minutes ago"
	}
	if d < 24*time.Hour {
		hours := int(d.Round(time.Hour) / time.Hour)
		if hours <= 1 {
			return "One hour ago"
		}
		return upperFirst(numberWord(hours)) + " hours ago"
	}
	days := int(d.Round(24*time.Hour) / (24 * time.Hour))
	if days <= 1 {
		return "One day ago"
	}
	return upperFirst(numberWord(days)) + " days ago"
}

// numberWord returns the lowercase English word for small numbers, or the decimal string.
func numberWord(n int) string {
	if n < 0 {
		return fmt.Sprintf("%d", n)
	}
	switch n {
	case 1:
		return "one"
	case 2:
		return "two"
	case 3:
		return "three"
	case 4:
		return "four"
	case 5:
		return "five"
	case 6:
		return "six"
	case 7:
		return "seven"
	case 8:
		return "eight"
	case 9:
		return "nine"
	case 10:
		return "ten"
	case 11:
		return "eleven"
	case 12:
		return "twelve"
	case 13:
		return "thirteen"
	case 14:
		return "fourteen"
	case 15:
		return "fifteen"
	case 16:
		return "sixteen"
	case 17:
		return "seventeen"
	case 18:
		return "eighteen"
	case 19:
		return "nineteen"
	case 20:
		return "twenty"
	case 30:
		return "thirty"
	case 40:
		return "forty"
	case 50:
		return "fifty"
	}
	if n > 20 && n < 60 {
		tens := (n / 10) * 10
		units := n % 10
		if units == 0 {
			return numberWord(tens)
		}
		return numberWord(tens) + "-" + numberWord(units)
	}
	return fmt.Sprintf("%d", n)
}

// upperFirst capitalizes the first character of text.
func upperFirst(text string) string {
	if text == "" {
		return ""
	}
	runes := []rune(text)
	runes[0] = unicode.ToUpper(runes[0])
	return string(runes)
}

// SpeechRate implements the per-result rate hook the pipeline reads after a
// successful Execute. Zero means "use the setting".
func (o RepeatOut) SpeechRate() float64 { return o.Rate }

// HelpIn optionally names a topic. A non-empty Topic narrows the spoken answer
// to the one area that matches, or names the areas when the topic is unknown.
type HelpIn struct {
	Topic string `json:"topic,omitempty"`
}

// HelpTool describes one enabled Tool. The help result is Local-only, so it
// may name Local-only Tools without exposing them to the Agent path.
type HelpTool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Permission  string `json:"permission"`
	LocalOnly   bool   `json:"local_only,omitempty"`
}

// HelpOut is the generated capability list and the one-line spoken form.
type HelpOut struct {
	Tools []HelpTool `json:"tools"`
	Text  string     `json:"text"`
}

// NewRepeat builds assistant.repeat. It reads the shared last-reply store at
// call time; a nil store or an empty history is a clear failure, never a
// repeated echo of the request itself. The optional rateSource returns the
// live voice.rate setting, used as the base when the last reply had no
// override; the variadic form keeps older callers source-compatible.
func NewRepeat(store *lastreply.Store, rateSource ...func() float64) tool.Tool[RepeatIn, RepeatOut] {
	var current func() float64
	if len(rateSource) > 0 {
		current = rateSource[0]
	}
	currentRate := func() float64 {
		if current == nil {
			return 1.0
		}
		if rate := current(); rate > 0 {
			return rate
		}
		return 1.0
	}
	return tool.Tool[RepeatIn, RepeatOut]{
		Name:        RepeatName,
		Description: "Repeat the last reply the Assistant gave, optionally slower or faster.",
		Permission:  tool.PermissionRead,
		// The previous reply may be a Local-only result, and the Agent path
		// must never see it as Tool output. Declaring the repeat Tool itself
		// Local-only is the explicit, safe choice.
		LocalOnly: tool.LocalOnlyYes,
		// The repeated text keeps the flag it had when it was spoken. This is
		// the one declared per-result exception, sitting next to LocalOnly so
		// it is reviewed with the privacy decision; it reports only the text
		// repeat replays, never new text of its own.
		Provenance: func(out RepeatOut) (bool, bool) { return out.LocalOnly, true },
		Handler: func(_ context.Context, in RepeatIn) (RepeatOut, error) {
			reply, ok := store.Last()
			if !ok || strings.TrimSpace(reply.Text) == "" {
				return RepeatOut{}, &tool.ToolError{Say: "I haven't said anything yet."}
			}
			return RepeatOut{
				Text:      reply.Text,
				Tool:      reply.Tool,
				Path:      reply.Path,
				LocalOnly: reply.LocalOnly,
				At:        reply.At,
				Rate:      repeatRate(in.Rate, reply.SpeechRate, currentRate),
			}, nil
		},
		Speak: func(out RepeatOut) string { return out.Text },
		Examples: []string{
			"say that again",
			"what did you say",
			"repeat that",
			"can you repeat that",
			"say it again",
			"what was that",
			"I missed that",
			"come again",
			"say that again slower",
			"say that again faster",
			"again but slower",
			"again but faster",
			"a bit slower",
			"say it again more slowly",
			"repeat that faster please",
			"at half speed",
			"at double speed",
		},
		Extract: extractRepeatRate,
	}
}

// repeatRate computes the per-utterance multiplier for one replay. An empty
// direction keeps the setting (0). A step is 20% of the rate the previous reply
// actually used, falling back to the current setting when it had no override.
// "half"/"double" are absolute. The result is clamped to the shared TTS range
// and rounded so repeated steps do not accumulate float noise; the persistent
// voice.rate setting is never touched.
func repeatRate(direction string, previous float64, current func() float64) float64 {
	if direction == "" {
		return 0
	}
	base := previous
	if base <= 0 && current != nil {
		base = current()
	}
	if base <= 0 {
		base = 1.0
	}
	var rate float64
	switch direction {
	case RepeatHalf:
		rate = wyoming.MinRate
	case RepeatDouble:
		rate = wyoming.MaxRate
	case RepeatSlower:
		rate = base * (1 - repeatStep)
	case RepeatFaster:
		rate = base * (1 + repeatStep)
	default:
		return 0
	}
	if rate < wyoming.MinRate {
		rate = wyoming.MinRate
	}
	if rate > wyoming.MaxRate {
		rate = wyoming.MaxRate
	}
	return math.Round(rate*100) / 100
}

// extractRepeatRate reads a speech-rate direction from a repeat utterance. Any
// repeat utterance is a match (an absent direction keeps the setting), so the
// semantic Fast path can still route a plain "say that again". It deliberately
// does not match music/playback speed, which belongs to other Tools.
func extractRepeatRate(text string) (RepeatIn, bool) {
	lower := strings.ToLower(text)
	for _, other := range []string{"music", "song", "playlist", "volume", "fan", "video"} {
		if strings.Contains(lower, other) {
			return RepeatIn{}, false
		}
	}
	switch {
	case strings.Contains(lower, "half speed"), strings.Contains(lower, "half as fast"), strings.Contains(lower, "half the speed"):
		return RepeatIn{Rate: RepeatHalf}, true
	case strings.Contains(lower, "double speed"), strings.Contains(lower, "twice as fast"), strings.Contains(lower, "double the speed"):
		return RepeatIn{Rate: RepeatDouble}, true
	case strings.Contains(lower, "slower"), strings.Contains(lower, "more slowly"), strings.Contains(lower, "slowly"):
		return RepeatIn{Rate: RepeatSlower}, true
	case strings.Contains(lower, "faster"), strings.Contains(lower, "more quickly"), strings.Contains(lower, "quicker"):
		return RepeatIn{Rate: RepeatFaster}, true
	default:
		return RepeatIn{}, true
	}
}

// NewHeard builds assistant.heard. It reads the shared last-reply store at
// call time; an empty store or blank transcript says "I haven't heard anything yet."
// It is deliberately Local-only: a transcript can be private and must never
// leave the Pi.
func NewHeard(store *lastreply.Store) tool.Tool[HeardIn, HeardOut] {
	return tool.Tool[HeardIn, HeardOut]{
		Name:        HeardName,
		Description: "Read back the transcript the Assistant acted on.",
		Permission:  tool.PermissionRead,
		LocalOnly:   tool.LocalOnlyYes,
		Handler: func(_ context.Context, _ HeardIn) (HeardOut, error) {
			if store == nil {
				return HeardOut{}, &tool.ToolError{Say: "I haven't heard anything yet."}
			}
			reply, ok := store.Last()
			if !ok || strings.TrimSpace(reply.Utterance) == "" {
				return HeardOut{}, &tool.ToolError{Say: "I haven't heard anything yet."}
			}
			return HeardOut{
				Text:      fmt.Sprintf("I heard: %q.", reply.Utterance),
				Utterance: reply.Utterance,
				At:        reply.At,
				Source:    reply.Source,
			}, nil
		},
		Speak: func(out HeardOut) string { return out.Text },
		Examples: []string{
			"did you catch that",
			"what was my question",
			"read back what I said",
		},
	}
}

// NewHelp builds assistant.help. The handler walks the registry every call and
// keeps only Tools whose effective Enabled flag is true, so a Dashboard
// disable applies to the next request and no feature list lives in code.
func NewHelp(registry *tool.Registry) tool.Tool[HelpIn, HelpOut] {
	return tool.Tool[HelpIn, HelpOut]{
		Name:        HelpName,
		Description: "List the Tools that are enabled right now, or what I can do with one area.",
		Permission:  tool.PermissionRead,
		// The list would name Local-only Tools, which the Agent path and the
		// MCP surface must not learn. Keeping the whole result on the Pi is
		// simpler and stricter than filtering the list per caller.
		LocalOnly: tool.LocalOnlyYes,
		Handler: func(_ context.Context, in HelpIn) (HelpOut, error) {
			if registry == nil {
				return HelpOut{Text: "I don't have any tools enabled right now."}, nil
			}
			names := registry.Names()
			out := HelpOut{Tools: make([]HelpTool, 0, len(names))}
			enabled := make([]HelpTool, 0, len(names))
			for _, name := range names {
				info, ok := registry.Info(name)
				if !ok || !info.Enabled {
					continue
				}
				ht := HelpTool{
					Name:        info.Name,
					Description: info.Description,
					Permission:  string(info.Permission),
					LocalOnly:   info.LocalOnly,
				}
				out.Tools = append(out.Tools, ht)
				enabled = append(enabled, ht)
			}
			out.Text = helpText(enabled, in.Topic)
			return out, nil
		},
		Speak: func(out HelpOut) string { return out.Text },
		Extract: extractHelpTopic,
		Examples: []string{
			"what can you do",
			"what can you help with",
			"what are your capabilities",
			"what are you able to do",
			"list your tools",
			"what tools do you have",
			"help",
			"tell me what you can do",
			"what can you do with timers",
			"what can you do with the weather",
		},
	}
}

// helpRuneBudget is the hard upper bound on the spoken "what can you do" reply.
// A clause that would cross it is dropped whole; the text is never truncated
// mid-word. The full list stays on the Dashboard Routes page, so the spoken
// answer only has to point at the areas.
const (
	helpRuneBudget     = 400
	helpMaxAreas       = 6
	helpTopicInvitation = "Ask what I can do with one of them."
	helpTopicBudget    = 300
)

// moduleArea maps a module name to a short spoken clause. It is presentation
// only, not a feature list: a module not in the map falls back to its own name,
// so adding a module needs no edit here.
var moduleArea = map[string]string{
	"timer":    "set timers, alarms and reminders",
	"alarm":    "set timers, alarms and reminders",
	"reminder": "set timers, alarms and reminders",
	"weather":  "check the weather",
	"list":     "keep lists and notes",
	"note":     "keep lists and notes",
	"music":    "play music",
	"lights":   "control the lights",
	"github":   "search the web and GitHub",
	"web":      "search the web and GitHub",
}

// areaPriority fixes the spoken order of the known areas. Areas outside this list
// follow alphabetically so the answer is stable without editing this package.
var areaPriority = []string{
	"set timers, alarms and reminders",
	"check the weather",
	"keep lists and notes",
	"play music",
	"control the lights",
	"search the web and GitHub",
}

// helpText is the bounded, grouped spoken capability answer. A known topic lists
// only that area's Tools; an unknown topic or no topic names the areas.
func helpText(tools []HelpTool, topic string) string {
	if len(tools) == 0 {
		return "I don't have any tools enabled right now."
	}
	if topic != "" {
		if text, ok := helpForTopic(tools, topic); ok {
			return text
		}
	}
	return helpSummary(tools)
}

// helpSummary names the capability areas in priority order, at most helpMaxAreas
// clauses, finishing with the topic invitation and never exceeding the budget.
func helpSummary(tools []HelpTool) string {
	clauses := areaClauses(distinctAreas(tools))
	for {
		text := renderClauses(clauses)
		if len([]rune(text)) <= helpRuneBudget || len(clauses) <= 1 {
			return text
		}
		clauses = clauses[:len(clauses)-1]
	}
}

// renderClauses turns the chosen area clauses into the spoken sentence.
func renderClauses(clauses []string) string {
	if len(clauses) == 0 {
		return helpTopicInvitation
	}
	body := strings.Join(clauses, ", ")
	last := clauses[len(clauses)-1]
	if last == "and more" {
		// The marker already begins with "and"; a plain list separator keeps it
		// from reading as a doubled "and and more".
		return "I can " + body + ". " + helpTopicInvitation
	}
	// Oxford-comma the final join so the list reads as a list, not a run-on.
	if i := strings.LastIndex(body, ", "); i >= 0 {
		body = body[:i] + ", and " + body[i+2:]
	}
	return "I can " + body + ". " + helpTopicInvitation
}

// areaClauses returns up to helpMaxAreas clauses, reserving the last slot for an
// "and more" marker when there are more areas than fit.
func areaClauses(areas []string) []string {
	if len(areas) <= helpMaxAreas {
		return append([]string(nil), areas...)
	}
	clauses := append([]string(nil), areas[:helpMaxAreas-1]...)
	clauses = append(clauses, "and more")
	return clauses
}

// distinctAreas returns the present area clauses in fixed priority order, then
// alphabetically for any area outside the known set.
func distinctAreas(tools []HelpTool) []string {
	seen := make(map[string]bool)
	var areas []string
	for _, tool := range tools {
		area := areaClauseFor(tool)
		if !seen[area] {
			seen[area] = true
			areas = append(areas, area)
		}
	}
	sort.SliceStable(areas, func(i, j int) bool {
		pi, pj := areaOrder(areas[i]), areaOrder(areas[j])
		if pi != pj {
			return pi < pj
		}
		return areas[i] < areas[j]
	})
	return areas
}

// areaClauseFor turns one Tool into its spoken area clause.
func areaClauseFor(tool HelpTool) string {
	if phrase, ok := moduleArea[moduleOf(tool.Name)]; ok {
		return phrase
	}
	return moduleOf(tool.Name)
}

// areaOrder is the priority rank of an area clause; known areas come first.
func areaOrder(clause string) int {
	for i, known := range areaPriority {
		if known == clause {
			return i
		}
	}
	return len(areaPriority)
}

// moduleOf returns the module name (the part of a canonical name before the
// first dot), so timer.list, alarm.set and reminder.set all group together.
func moduleOf(name string) string {
	if i := strings.Index(name, "."); i >= 0 {
		return name[:i]
	}
	return name
}

// helpForTopic answers a known topic by listing only the matching area's Tools
// with their own descriptions, capped at helpTopicBudget runes. An unknown topic
// returns ok=false so the caller names the areas instead.
func helpForTopic(tools []HelpTool, topic string) (string, bool) {
	norm := normalizeTopic(topic)
	if norm == "" {
		return "", false
	}
	var b strings.Builder
	total := 0
	matched := false
	for _, tool := range tools {
		if !topicMatches(norm, tool) {
			continue
		}
		matched = true
		description := strings.TrimSpace(tool.Description)
		if description == "" {
			continue
		}
		if total+len([]rune(description)) > helpTopicBudget {
			break
		}
		if b.Len() > 0 {
			b.WriteString(" ")
		}
		b.WriteString(description)
		total += len([]rune(description))
	}
	if !matched || b.Len() == 0 {
		return "", false
	}
	return b.String(), true
}

// topicMatches reports whether a normalized topic refers to a Tool's area,
// module, canonical domain prefix or name. Matching is case-insensitive and
// token-based after shared normalisation, so "timers" finds the timer area and
// "weather" finds the weather module without inventing a match.
func topicMatches(norm string, tool HelpTool) bool {
	topicTokens := tokenize(norm)
	if len(topicTokens) == 0 {
		return false
	}
	targets := []string{
		moduleOf(tool.Name),
		tool.Name,
		areaClauseFor(tool),
	}
	for _, token := range topicTokens {
		for _, target := range targets {
			for _, targetToken := range tokenize(target) {
				if targetToken == token {
					return true
				}
				// Tolerate singular/plural drift for non-trivial tokens so
				// "light" still finds the lights area without matching "e".
				if len(token) >= 4 && strings.HasPrefix(targetToken, token) {
					return true
				}
			}
		}
	}
	return false
}

// normalizeTopic lowercases, trims punctuation and drops a leading article.
func normalizeTopic(topic string) string {
	t := strings.ToLower(strings.TrimSpace(topic))
	t = strings.Trim(t, " .,!?;:")
	t = strings.TrimPrefix(t, "the ")
	t = strings.TrimPrefix(t, "a ")
	t = strings.TrimPrefix(t, "an ")
	return strings.TrimSpace(t)
}

// tokenize splits text into lowercase letter/number words.
func tokenize(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
}

// extractHelpTopic is the Fast-path argument filler. It claims only help-style
// utterances so a topic such as "what can you do with the weather" still reaches
// the help Tool; a topic phrase after with/for/about narrows the answer.
func extractHelpTopic(text string) (HelpIn, bool) {
	lower := strings.ToLower(text)
	if !isHelpUtterance(lower) {
		return HelpIn{}, false
	}
	return HelpIn{Topic: extractTopicPhrase(lower)}, true
}

// isHelpUtterance recognises the capability-discovery phrasing the help Tool
// owns. It deliberately does not match tool-specific verbs, so "set a timer"
// never resolves here.
func isHelpUtterance(lower string) bool {
	anchors := []string{
		"what can you do",
		"what can you help",
		"what are your capabilities",
		"what are you able",
		"what are you capable",
		"list your tools",
		"what tools do you",
		"tell me what you can",
		"what do you do",
		"what can i ask",
		"what kind of things",
		"what functions",
		"show me your tools",
		"give me a list of your tools",
		"help",
	}
	for _, anchor := range anchors {
		if strings.Contains(lower, anchor) {
			return true
		}
	}
	return false
}

// extractTopicPhrase pulls the words after with/for/about as the topic.
func extractTopicPhrase(lower string) string {
	for _, marker := range []string{" with ", " for ", " about "} {
		if i := strings.Index(lower, marker); i >= 0 {
			return strings.TrimSpace(lower[i+len(marker):])
		}
	}
	return ""
}
