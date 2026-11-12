package quiet

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/parse"
	quietcore "github.com/Jairik/clanka-bot/assistant/internal/quiet"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// SetIn is one daily Quiet window. The times are local clock strings in the
// effective timezone; an end earlier than the start crosses midnight.
type SetIn struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

// SetOut echoes the stored window.
type SetOut struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

// OffIn has no arguments.
type OffIn struct{}

// OffOut reports that the schedule was turned off.
type OffOut struct {
	Off bool `json:"off"`
}

// DNDIn is one timed do-not-disturb period.
type DNDIn struct {
	DurationSeconds int `json:"duration_seconds"`
}

// DNDOut echoes the absolute end of the period.
type DNDOut struct {
	DurationSeconds int    `json:"duration_seconds"`
	Until           string `json:"until"`
}

// AwakeIn has no arguments.
type AwakeIn struct{}

// AwakeOut reports what the wake-up acknowledgement stopped.
type AwakeOut struct {
	// Bypassed is true when a daily window was active and is now skipped.
	Bypassed bool `json:"bypassed"`
	// DoNotDisturb is true when a timed do-not-disturb period was cleared.
	DoNotDisturb bool `json:"do_not_disturb"`
}

// NewSet builds quiet.set. It writes the same overrides the Dashboard edits.
func NewSet(service *quietcore.Service) tool.Tool[SetIn, SetOut] {
	return tool.Tool[SetIn, SetOut]{
		Name:        SetName,
		Description: "Set the daily Quiet hours window, such as from 10:30 pm to 7 am.",
		Permission:  tool.PermissionAct,
		// Quiet hours are local-only: the window and volume stay on the Pi.
		LocalOnly: tool.LocalOnlyYes,
		Handler: func(ctx context.Context, in SetIn) (SetOut, error) {
			if service == nil {
				return SetOut{}, &tool.ToolError{Say: "Quiet hours are not available."}
			}
			status, err := service.SetWindow(ctx, in.Start, in.End)
			if err != nil {
				return SetOut{}, &tool.ToolError{Say: "I couldn't set quiet hours: " + err.Error()}
			}
			return SetOut{Start: status.Start, End: status.End}, nil
		},
		Speak: func(out SetOut) string {
			return "Quiet hours set from " + quietcore.HumanClock(out.Start) + " to " + quietcore.HumanClock(out.End) + "."
		},
		Examples: []string{
			"set quiet hours from 10:30 to 7",
			"set quiet hours from 11 pm to 6 am",
			"set quiet hours from 9 at night to 6 in the morning",
			"set my quiet hours from 10 pm to 7 am",
			"quiet hours from 11:30 pm until 6:30 am",
			"quiet hours from 10 at night to 7 in the morning",
			"change quiet hours to 10 pm to 7 am",
			"make quiet hours from 10:30 to 7",
			"set the quiet window from 9 pm to 6 am",
			"quiet hours should run from 11 to 7",
			"configure quiet hours from 10 pm until 7 am",
			"start quiet hours at 10 pm and end at 7",
			"quiet hours every night from 10:30 to 7",
			"I want quiet hours from 11 pm to 7 am",
			"set quiet time from 10:30 pm to 7 am",
			"quiet hours begin at 10 and end at 6",
			"change my quiet hours to 11 pm to 6 am",
			"set nightly quiet hours from 10:30 to 7",
			"quiet hours from 9 in the evening to 6 in the morning",
			"quiet hours from 10:30 in the evening to 7 in the morning",
		},
		Extract: func(text string) (SetIn, bool) {
			start, end, ok := parseWindow(text)
			if !ok {
				return SetIn{}, false
			}
			return SetIn{Start: start, End: end}, true
		},
	}
}

// NewOff builds quiet.off. It also clears a timed do-not-disturb and awake
// bypass, so "turn off quiet hours" really means all Quiet behaviour is off.
func NewOff(service *quietcore.Service) tool.Tool[OffIn, OffOut] {
	return tool.Tool[OffIn, OffOut]{
		Name:        OffName,
		Description: "Turn Quiet hours off and clear any do-not-disturb or awake bypass.",
		Permission:  tool.PermissionAct,
		LocalOnly:   tool.LocalOnlyYes,
		Handler: func(ctx context.Context, _ OffIn) (OffOut, error) {
			if service == nil {
				return OffOut{}, &tool.ToolError{Say: "Quiet hours are not available."}
			}
			if err := service.Disable(ctx); err != nil {
				return OffOut{}, &tool.ToolError{Say: "I couldn't turn quiet hours off: " + err.Error()}
			}
			return OffOut{Off: true}, nil
		},
		Speak: func(OffOut) string { return "Quiet hours off." },
		Examples: []string{
			"turn off quiet hours",
			"quiet hours off",
			"stop quiet hours",
			"turn quiet hours off",
			"cancel quiet hours",
			"end quiet hours",
			"quiet hours are over",
			"I don't want quiet hours anymore",
			"remove quiet hours",
			"quiet hours aren't needed",
			"deactivate quiet hours",
			"turn off the quiet window",
			"no more quiet hours",
			"quiet hours should be disabled",
			"stop the quiet schedule",
			"disable my quiet hours",
		},
		Extract: extractOff,
	}
}

// NewDND builds quiet.dnd. The duration is turned into an absolute stored
// instant, so a restart does not restart the countdown.
func NewDND(service *quietcore.Service) tool.Tool[DNDIn, DNDOut] {
	return tool.Tool[DNDIn, DNDOut]{
		Name:        DNDName,
		Description: "Start a timed do-not-disturb period, such as for two hours.",
		Permission:  tool.PermissionAct,
		LocalOnly:   tool.LocalOnlyYes,
		Handler: func(ctx context.Context, in DNDIn) (DNDOut, error) {
			if service == nil {
				return DNDOut{}, &tool.ToolError{Say: "Do not disturb is not available."}
			}
			if in.DurationSeconds <= 0 {
				return DNDOut{}, &tool.ToolError{Say: "How long should do not disturb last?"}
			}
			duration := time.Duration(in.DurationSeconds) * time.Second
			until, err := service.StartDND(ctx, duration)
			if err != nil {
				return DNDOut{}, &tool.ToolError{Say: "I couldn't start do not disturb: " + err.Error()}
			}
			return DNDOut{DurationSeconds: in.DurationSeconds, Until: until.Format(time.RFC3339)}, nil
		},
		Speak: func(out DNDOut) string {
			return "Do not disturb for " + parse.HumanDuration(time.Duration(out.DurationSeconds)*time.Second) + "."
		},
		Examples: []string{
			"do not disturb for two hours",
			"do not disturb for 30 minutes",
			"turn on do not disturb for an hour",
			"mute everything for an hour",
			"dnd for 45 minutes",
			"be quiet for the next two hours",
			"do not disturb me for 90 minutes",
			"silence things for 20 minutes",
			"quiet down for the next hour",
			"hold everything for two hours",
			"set do not disturb for 15 minutes",
			"turn on dnd for two hours",
			"pause notifications for 10 minutes",
			"keep things quiet for 30 minutes",
			"I need quiet for the next two hours",
			"turn on do not disturb mode for 1 hour",
			"start do not disturb for 3 hours",
			"give me two hours of quiet",
			"mute the house for an hour",
			"do not disturb for 20 minutes",
		},
		Extract: extractDND,
	}
}

// NewAwake builds quiet.awake. It clears do-not-disturb and bypasses only the
// rest of the current daily window; the schedule itself stays on.
func NewAwake(service *quietcore.Service) tool.Tool[AwakeIn, AwakeOut] {
	return tool.Tool[AwakeIn, AwakeOut]{
		Name:        AwakeName,
		Description: "End the current Quiet hours early and go back to normal volume.",
		Permission:  tool.PermissionAct,
		LocalOnly:   tool.LocalOnlyYes,
		Handler: func(ctx context.Context, _ AwakeIn) (AwakeOut, error) {
			if service == nil {
				return AwakeOut{}, &tool.ToolError{Say: "Quiet hours are not available."}
			}
			before, err := service.Awake(ctx)
			if err != nil {
				return AwakeOut{}, &tool.ToolError{Say: "I couldn't end quiet hours: " + err.Error()}
			}
			return AwakeOut{
				Bypassed:     before.InWindow,
				DoNotDisturb: before.Reason == quietcore.ReasonDND,
			}, nil
		},
		Speak: func(out AwakeOut) string {
			switch {
			case out.Bypassed:
				return "Okay, I'm awake. Quiet hours are off until the window ends."
			case out.DoNotDisturb:
				return "Okay, I'm awake. Do not disturb is off."
			default:
				return "It's not quiet now."
			}
		},
		Examples: []string{
			"I'm awake",
			"I am awake",
			"I'm up",
			"I am up now",
			"I'm awake now",
			"I'm already awake",
			"I'm up for the day",
			"I'm awake, you can talk normally",
			"I'm awake and up",
			"I'm awake, stop whispering",
			"stop whispering, I'm awake",
			"I'm awake, speak normally",
			"I'm awake, no need to whisper",
			"awake now",
			"I'm awake, normal volume please",
			"I'm awake, you can be loud again",
			"I'm awake, back to normal",
			"I'm up now, no more whispering",
			"I'm awake, end the quiet please",
			"I'm awake, quiet time is over",
		},
		Extract: extractAwake,
	}
}

// clockToken finds a spoken clock time. The optional meridiem is part of the
// match so "10 pm" and "10" are treated differently.
var clockToken = regexp.MustCompile(`(?i)\b([0-9]{1,2})(?::([0-9]{2}))?\s*(a\.?m\.?|p\.?m\.?)?`)

// parseWindow reads two clock times from a Quiet-hours utterance. It applies
// one deliberate heuristic: "from 10:30 to 7" means 10:30 pm, not a 20.5-hour
// daytime window, so a reversed pair of 12-hour clocks moves the start into
// the evening.
func parseWindow(text string) (string, string, bool) {
	lower := strings.ToLower(text)
	if !strings.Contains(lower, "quiet") && !strings.Contains(lower, "window") {
		return "", "", false
	}
	matches := clockToken.FindAllStringSubmatch(text, -1)
	if len(matches) < 2 {
		return "", "", false
	}
	start, startOK := matchClock(matches[0])
	end, endOK := matchClock(matches[1])
	if !startOK || !endOK {
		return "", "", false
	}
	startHour, startMinute, _ := splitClock(start)
	endHour, endMinute, _ := splitClock(end)
	startHadMeridiem := strings.TrimSpace(matches[0][3]) != ""
	// Reversed 12-hour clocks without an explicit start am/pm: 10:30 -> 7
	// becomes 22:30 -> 7. Midnight is special because 12 already means noon.
	if !startHadMeridiem {
		switch {
		case startHour == 12 && endHour < 12:
			startHour = 0
			start = fmt.Sprintf("%02d:%02d", startHour, startMinute)
		case startHour < 12 && endHour < 12 && (startHour > endHour || (startHour == endHour && startMinute > endMinute)):
			startHour += 12
			start = fmt.Sprintf("%02d:%02d", startHour, startMinute)
		}
	}
	if start == end {
		return "", "", false
	}
	return start, end, true
}

// matchClock converts one regex match into a normalised 24-hour clock.
func matchClock(match []string) (string, bool) {
	hourText := match[1]
	minuteText := ""
	if len(match) > 2 {
		minuteText = match[2]
	}
	meridiem := ""
	if len(match) > 3 {
		meridiem = strings.ToLower(strings.ReplaceAll(match[3], ".", ""))
	}
	raw := hourText
	if minuteText != "" {
		raw += ":" + minuteText
	}
	if meridiem != "" {
		raw += " " + meridiem
	}
	return quietcore.ParseClock(raw)
}

// splitClock splits one normalised HH:MM clock.
func splitClock(value string) (hour, minute int, ok bool) {
	if _, err := fmt.Sscanf(value, "%d:%d", &hour, &minute); err != nil {
		return 0, 0, false
	}
	return hour, minute, true
}

// extractOff recognises the explicit ways to disable the schedule. A window
// utterance is rejected so "set quiet hours ..." cannot fall here.
func extractOff(text string) (OffIn, bool) {
	lower := strings.ToLower(text)
	if _, _, ok := parseWindow(text); ok {
		return OffIn{}, false
	}
	// Require the Quiet schedule, not any phrase containing "quiet" (for
	// example "turn off the quiet music" must stay an audio request).
	if !strings.Contains(lower, "quiet hours") &&
		!strings.Contains(lower, "quiet window") &&
		!strings.Contains(lower, "quiet schedule") &&
		!strings.Contains(lower, "quiet time") {
		return OffIn{}, false
	}
	verbs := []string{
		"turn off", "switch off", "disable", "deactivate", "stop", "end",
		"cancel", "remove", "no more", "get rid of", "aren't needed",
		"don't want", "should be disabled", "are over", "quiet hours off",
	}
	for _, verb := range verbs {
		if strings.Contains(lower, verb) {
			return OffIn{}, true
		}
	}
	return OffIn{}, false
}

// extractDND requires a do-not-disturb trigger and a duration. A daily window
// with two clock times is rejected so quiet.set keeps its own phrasing.
func extractDND(text string) (DNDIn, bool) {
	lower := strings.ToLower(text)
	if _, _, ok := parseWindow(text); ok {
		return DNDIn{}, false
	}
	triggers := []string{
		"do not disturb", "dnd", "mute everything", "mute the house",
		"silence things", "quiet down", "hold everything", "pause notifications",
		"keep things quiet", "need quiet", "give me", "be quiet for",
	}
	triggered := false
	for _, trigger := range triggers {
		if !strings.Contains(lower, trigger) {
			continue
		}
		// "give me" is only a DND cue when it asks for quiet, so "give me a
		// timer for five minutes" cannot become a do-not-disturb period.
		if trigger == "give me" && !strings.Contains(lower, "quiet") {
			continue
		}
		triggered = true
		break
	}
	if !triggered {
		return DNDIn{}, false
	}
	duration, ok := dndDuration(text)
	if !ok || duration <= 0 {
		return DNDIn{}, false
	}
	return DNDIn{DurationSeconds: int(duration.Round(time.Second) / time.Second)}, true
}

// dndDuration reads the period, including the common "for the next hour"
// phrasing that parse.Duration does not attach a number to.
func dndDuration(text string) (time.Duration, bool) {
	if duration, ok := parse.Duration(text, time.Hour); ok && duration > 0 {
		return duration, true
	}
	lower := strings.ToLower(text)
	for _, phrase := range []string{"the next hour", "the coming hour", "the rest of the hour"} {
		if strings.Contains(lower, phrase) {
			return time.Hour, true
		}
	}
	return 0, false
}

// extractAwake recognises the short wake-up phrases. It deliberately does not
// require the word "quiet": "I'm awake" is the whole phrase.
func extractAwake(text string) (AwakeIn, bool) {
	lower := strings.ToLower(text)
	phrases := []string{
		"i'm awake", "i am awake", "im awake", "already awake", "i'm up", "i am up",
		"awake now", "stop whispering", "no more whispering",
	}
	for _, phrase := range phrases {
		if strings.Contains(lower, phrase) {
			return AwakeIn{}, true
		}
	}
	return AwakeIn{}, false
}
