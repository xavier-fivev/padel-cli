package cmd

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

type CalendarEvent struct {
	Start   time.Time
	End     time.Time
	Summary string
}

func fetchICalendar(ctx context.Context, feedURL string, loc *time.Location) ([]CalendarEvent, error) {
	feedURL = strings.TrimSpace(feedURL)
	if feedURL == "" {
		return nil, nil
	}
	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, feedURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/calendar,*/*")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("fetch calendar failed: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return parseICalendar(string(body), loc)
}

func parseICalendar(input string, loc *time.Location) ([]CalendarEvent, error) {
	lines := unfoldICalendarLines(input)
	events := []CalendarEvent{}
	inEvent := false
	current := CalendarEvent{}
	var hasStart bool
	rrule := ""

	for _, line := range lines {
		name, params, value, ok := parseICalendarProperty(line)
		if !ok {
			continue
		}
		switch name {
		case "BEGIN":
			if strings.EqualFold(value, "VEVENT") {
				inEvent = true
				current = CalendarEvent{}
				hasStart = false
				rrule = ""
			}
		case "END":
			if strings.EqualFold(value, "VEVENT") && inEvent {
				if !hasStart {
					return nil, fmt.Errorf("calendar event missing DTSTART")
				}
				if current.End.IsZero() {
					current.End = current.Start.Add(time.Hour)
				}
				if !current.End.After(current.Start) {
					return nil, fmt.Errorf("calendar event ends before it starts")
				}
				if rrule != "" {
					expanded, err := expandRecurringCalendarEvent(current, rrule, loc)
					if err != nil {
						return nil, err
					}
					events = append(events, expanded...)
				} else {
					events = append(events, current)
				}
				inEvent = false
			}
		}
		if !inEvent {
			continue
		}

		switch name {
		case "DTSTART":
			parsed, allDay, err := parseICalendarTime(value, params, loc)
			if err != nil {
				return nil, fmt.Errorf("parse DTSTART: %w", err)
			}
			current.Start = parsed
			hasStart = true
			if allDay && current.End.IsZero() {
				current.End = parsed.Add(24 * time.Hour)
			}
		case "DTEND":
			parsed, _, err := parseICalendarTime(value, params, loc)
			if err != nil {
				return nil, fmt.Errorf("parse DTEND: %w", err)
			}
			current.End = parsed
		case "SUMMARY":
			current.Summary = value
		case "RRULE":
			rrule = value
		case "RDATE":
			return nil, fmt.Errorf("calendar RDATE recurrence is not supported")
		}
	}
	if inEvent {
		return nil, fmt.Errorf("calendar ended inside VEVENT")
	}
	return events, nil
}

func expandRecurringCalendarEvent(event CalendarEvent, rrule string, loc *time.Location) ([]CalendarEvent, error) {
	parts := parseRRuleParts(rrule)
	freq := strings.ToUpper(parts["FREQ"])
	if freq == "" {
		return nil, fmt.Errorf("calendar RRULE missing FREQ")
	}
	interval := parsePositiveRRuleInt(parts["INTERVAL"], 1)
	count := parsePositiveRRuleInt(parts["COUNT"], 0)

	until := time.Time{}
	if rawUntil := strings.TrimSpace(parts["UNTIL"]); rawUntil != "" {
		parsed, _, err := parseICalendarTime(rawUntil, map[string]string{}, loc)
		if err != nil {
			return nil, fmt.Errorf("parse RRULE UNTIL: %w", err)
		}
		until = parsed
	}
	if until.IsZero() && count == 0 {
		until = time.Now().In(loc).AddDate(1, 0, 0)
	}

	duration := event.End.Sub(event.Start)
	occurrences := []CalendarEvent{}
	appendOccurrence := func(start time.Time) bool {
		if start.Before(event.Start) {
			return false
		}
		if !until.IsZero() && start.After(until) {
			return true
		}
		occurrence := event
		occurrence.Start = start
		occurrence.End = start.Add(duration)
		occurrences = append(occurrences, occurrence)
		if count > 0 && len(occurrences) >= count {
			return true
		}
		return len(occurrences) >= 1000
	}

	switch freq {
	case "DAILY":
		allowedDays, err := parseRRuleByDay(parts["BYDAY"])
		if err != nil {
			return nil, err
		}
		for start := event.Start; ; start = start.AddDate(0, 0, interval) {
			if len(allowedDays) == 0 || weekdayInSet(start.Weekday(), allowedDays) {
				if appendOccurrence(start) {
					break
				}
			}
			if !until.IsZero() && start.After(until) {
				break
			}
		}
	case "WEEKLY":
		allowedDays, err := parseRRuleByDay(parts["BYDAY"])
		if err != nil {
			return nil, err
		}
		if len(allowedDays) == 0 {
			allowedDays = []time.Weekday{event.Start.Weekday()}
		}
		sortWeekdays(allowedDays)
		for week := calendarWeekStart(event.Start); ; week = week.AddDate(0, 0, 7*interval) {
			stop := false
			for _, weekday := range allowedDays {
				start := calendarDateWithTime(week.AddDate(0, 0, weekdayOffsetFromMonday(weekday)), event.Start)
				if appendOccurrence(start) {
					stop = true
					break
				}
			}
			if stop {
				break
			}
			if !until.IsZero() && week.After(until.AddDate(0, 0, 7)) {
				break
			}
		}
	default:
		return nil, fmt.Errorf("calendar RRULE FREQ=%s is not supported", freq)
	}
	return occurrences, nil
}

func parseRRuleParts(rrule string) map[string]string {
	parts := map[string]string{}
	for _, part := range strings.Split(rrule, ";") {
		key, value, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		parts[strings.ToUpper(strings.TrimSpace(key))] = strings.TrimSpace(value)
	}
	return parts
}

func parsePositiveRRuleInt(input string, fallback int) int {
	if strings.TrimSpace(input) == "" {
		return fallback
	}
	value, err := strconv.Atoi(strings.TrimSpace(input))
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}

func parseRRuleByDay(input string) ([]time.Weekday, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return nil, nil
	}
	weekdays := []time.Weekday{}
	for _, raw := range strings.Split(input, ",") {
		label := strings.ToUpper(strings.TrimSpace(raw))
		if len(label) > 2 {
			label = label[len(label)-2:]
		}
		switch label {
		case "MO":
			weekdays = append(weekdays, time.Monday)
		case "TU":
			weekdays = append(weekdays, time.Tuesday)
		case "WE":
			weekdays = append(weekdays, time.Wednesday)
		case "TH":
			weekdays = append(weekdays, time.Thursday)
		case "FR":
			weekdays = append(weekdays, time.Friday)
		case "SA":
			weekdays = append(weekdays, time.Saturday)
		case "SU":
			weekdays = append(weekdays, time.Sunday)
		default:
			return nil, fmt.Errorf("unsupported RRULE BYDAY value %q", raw)
		}
	}
	return weekdays, nil
}

func weekdayInSet(weekday time.Weekday, weekdays []time.Weekday) bool {
	for _, candidate := range weekdays {
		if weekday == candidate {
			return true
		}
	}
	return false
}

func sortWeekdays(weekdays []time.Weekday) {
	sort.Slice(weekdays, func(i, j int) bool {
		return weekdayOffsetFromMonday(weekdays[i]) < weekdayOffsetFromMonday(weekdays[j])
	})
}

func calendarWeekStart(value time.Time) time.Time {
	local := value
	start := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, local.Location())
	return start.AddDate(0, 0, -weekdayOffsetFromMonday(local.Weekday()))
}

func weekdayOffsetFromMonday(weekday time.Weekday) int {
	return (int(weekday) + 6) % 7
}

func calendarDateWithTime(date time.Time, clock time.Time) time.Time {
	return time.Date(date.Year(), date.Month(), date.Day(), clock.Hour(), clock.Minute(), clock.Second(), clock.Nanosecond(), clock.Location())
}

func unfoldICalendarLines(input string) []string {
	input = strings.ReplaceAll(input, "\r\n", "\n")
	input = strings.ReplaceAll(input, "\r", "\n")
	raw := strings.Split(input, "\n")
	lines := []string{}
	for _, line := range raw {
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			if len(lines) == 0 {
				continue
			}
			lines[len(lines)-1] += strings.TrimLeft(line, " \t")
			continue
		}
		lines = append(lines, strings.TrimSpace(line))
	}
	return lines
}

func parseICalendarProperty(line string) (string, map[string]string, string, bool) {
	left, value, ok := strings.Cut(line, ":")
	if !ok {
		return "", nil, "", false
	}
	parts := strings.Split(left, ";")
	if len(parts) == 0 {
		return "", nil, "", false
	}
	name := strings.ToUpper(strings.TrimSpace(parts[0]))
	params := map[string]string{}
	for _, part := range parts[1:] {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		params[strings.ToUpper(strings.TrimSpace(k))] = strings.Trim(strings.TrimSpace(v), `"`)
	}
	return name, params, strings.TrimSpace(value), true
}

func parseICalendarTime(value string, params map[string]string, defaultLoc *time.Location) (time.Time, bool, error) {
	value = strings.TrimSpace(value)
	loc := defaultLoc
	if loc == nil {
		loc = time.Local
	}
	if tzid := strings.TrimSpace(params["TZID"]); tzid != "" {
		loaded, err := time.LoadLocation(tzid)
		if err != nil {
			return time.Time{}, false, err
		}
		loc = loaded
	}

	if strings.EqualFold(params["VALUE"], "DATE") || len(value) == len("20060102") {
		parsed, err := time.ParseInLocation("20060102", value, loc)
		return parsed, true, err
	}

	if strings.HasSuffix(value, "Z") {
		for _, layout := range []string{"20060102T150405Z", "20060102T1504Z"} {
			parsed, err := time.Parse(layout, value)
			if err == nil {
				return parsed, false, nil
			}
		}
		return time.Time{}, false, fmt.Errorf("invalid UTC date-time %q", value)
	}

	for _, layout := range []string{"20060102T150405", "20060102T1504"} {
		parsed, err := time.ParseInLocation(layout, value, loc)
		if err == nil {
			return parsed, false, nil
		}
	}
	return time.Time{}, false, fmt.Errorf("invalid date-time %q", value)
}

func calendarHasConflict(events []CalendarEvent, start, end time.Time) bool {
	for _, event := range events {
		if intervalsOverlap(start, end, event.Start, event.End) {
			return true
		}
	}
	return false
}

func intervalsOverlap(aStart, aEnd, bStart, bEnd time.Time) bool {
	return aStart.Before(bEnd) && bStart.Before(aEnd)
}
