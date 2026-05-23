package cmd

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	autoBookVenueName     = "Indoor Padel Australia Alexandria"
	autoBookTimezone      = "Australia/Sydney"
	autoBookReleaseTime   = "18:30"
	autoBookRetryUntil    = "18:35"
	autoBookDaysInAdvance = 14
	autoBookWeeklyCap     = 3
	autoBookDailyCap      = 1
)

type autoBookProfile struct {
	Name             string
	AllowedWeekdays  []time.Weekday
	AllowedDurations []int
	StartWindowFrom  string
	StartWindowTo    string
}

var autoBookProfiles = map[string]autoBookProfile{
	"weekday": {
		Name:             "weekday",
		AllowedWeekdays:  []time.Weekday{time.Monday, time.Tuesday, time.Wednesday, time.Thursday},
		AllowedDurations: []int{90},
		StartWindowFrom:  "18:30",
		StartWindowTo:    "20:00",
	},
	"weekend": {
		Name:             "weekend",
		AllowedWeekdays:  []time.Weekday{time.Saturday, time.Sunday},
		AllowedDurations: []int{90, 120},
		StartWindowFrom:  "10:00",
		StartWindowTo:    "18:00",
	},
}

type AutoBookConfig struct {
	Mode          string
	DryRun        bool
	Timezone      string
	Venue         AutoBookVenueConfig
	Release       AutoBookReleaseConfig
	Booking       AutoBookBookingConfig
	Payment       AutoBookPaymentConfig
	Calendar      AutoBookCalendarConfig
	Notifications AutoBookNotificationsConfig
	Polling       AutoBookPollingConfig
}

type AutoBookVenueConfig struct {
	ID        string
	NameExact string
}

type AutoBookReleaseConfig struct {
	DaysInAdvance int
	Time          string
	RetryUntil    string
}

type AutoBookBookingConfig struct {
	AllowedWeekdays    []time.Weekday
	AllowedDurations   []int
	StartWindow        AutoBookStartWindowConfig
	MaxBookingsPerWeek int
	MaxBookingsPerDay  int
	Players            int
}

type AutoBookStartWindowConfig struct {
	From string
	To   string
}

type AutoBookPaymentConfig struct {
	Method string
}

type AutoBookCalendarConfig struct {
	ICalURL string
}

type AutoBookNotificationsConfig struct {
	Telegram AutoBookTelegramConfig
	WhatsApp AutoBookWhatsAppConfig
}

type AutoBookTelegramConfig struct {
	Enabled     bool
	BotTokenEnv string
	ChatIDEnv   string
}

type AutoBookWhatsAppConfig struct {
	Enabled bool
}

type AutoBookPollingConfig struct {
	MinIntervalSeconds int
	MaxIntervalSeconds int
}

func defaultAutoBookConfig() AutoBookConfig {
	profile := autoBookProfiles["weekday"]
	return AutoBookConfig{
		Mode:     profile.Name,
		DryRun:   true,
		Timezone: autoBookTimezone,
		Venue: AutoBookVenueConfig{
			NameExact: autoBookVenueName,
		},
		Release: AutoBookReleaseConfig{
			DaysInAdvance: autoBookDaysInAdvance,
			Time:          autoBookReleaseTime,
			RetryUntil:    autoBookRetryUntil,
		},
		Booking: AutoBookBookingConfig{
			AllowedWeekdays:  append([]time.Weekday(nil), profile.AllowedWeekdays...),
			AllowedDurations: append([]int(nil), profile.AllowedDurations...),
			StartWindow: AutoBookStartWindowConfig{
				From: profile.StartWindowFrom,
				To:   profile.StartWindowTo,
			},
			MaxBookingsPerWeek: autoBookWeeklyCap,
			MaxBookingsPerDay:  autoBookDailyCap,
			Players:            4,
		},
		Payment: AutoBookPaymentConfig{
			Method: "MERCHANT_WALLET",
		},
		Notifications: AutoBookNotificationsConfig{
			Telegram: AutoBookTelegramConfig{
				BotTokenEnv: "TELEGRAM_BOT_TOKEN",
				ChatIDEnv:   "TELEGRAM_CHAT_ID",
			},
		},
		Polling: AutoBookPollingConfig{
			MinIntervalSeconds: 15,
			MaxIntervalSeconds: 30,
		},
	}
}

func loadAutoBookConfig(path string) (AutoBookConfig, error) {
	if strings.TrimSpace(path) == "" {
		return AutoBookConfig{}, fmt.Errorf("--config is required")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return AutoBookConfig{}, err
	}

	cfg := defaultAutoBookConfig()
	sawMode := false
	sawDurationMinutes := false
	sawAllowedDurations := false
	sawAllowedWeekdays := false
	sawStartWindow := false
	sections := map[int]string{}
	lines := strings.Split(string(data), "\n")
	for i, raw := range lines {
		line := strings.TrimRight(raw, "\r")
		line = stripYAMLComment(line)
		if strings.TrimSpace(line) == "" {
			continue
		}
		indent := countLeadingSpaces(line)
		if indent%2 != 0 {
			return AutoBookConfig{}, fmt.Errorf("%s:%d: indentation must use multiples of two spaces", path, i+1)
		}
		level := indent / 2
		trimmed := strings.TrimSpace(line)
		key, value, ok := strings.Cut(trimmed, ":")
		if !ok {
			return AutoBookConfig{}, fmt.Errorf("%s:%d: expected key: value", path, i+1)
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key == "" {
			return AutoBookConfig{}, fmt.Errorf("%s:%d: empty key", path, i+1)
		}

		pathParts := make([]string, 0, level+1)
		for sectionLevel := 0; sectionLevel < level; sectionLevel++ {
			section := sections[sectionLevel]
			if section == "" {
				return AutoBookConfig{}, fmt.Errorf("%s:%d: missing parent section for %q", path, i+1, key)
			}
			pathParts = append(pathParts, section)
		}

		for sectionLevel := level; sectionLevel < 8; sectionLevel++ {
			delete(sections, sectionLevel)
		}

		if value == "" {
			sections[level] = key
			continue
		}

		pathParts = append(pathParts, key)
		joinedKey := strings.Join(pathParts, ".")
		switch joinedKey {
		case "mode":
			sawMode = true
		case "booking.duration_minutes":
			sawDurationMinutes = true
		case "booking.allowed_durations":
			sawAllowedDurations = true
		case "booking.allowed_weekdays":
			sawAllowedWeekdays = true
		case "booking.start_window.from", "booking.start_window.to":
			sawStartWindow = true
		}

		if err := setAutoBookConfigValue(&cfg, pathParts, parseYAMLScalar(value)); err != nil {
			return AutoBookConfig{}, fmt.Errorf("%s:%d: %w", path, i+1, err)
		}
	}

	// Apply profile defaults for any fields the user didn't override. The
	// validator below still rejects values that go beyond the profile.
	profile, ok := autoBookProfiles[strings.ToLower(strings.TrimSpace(cfg.Mode))]
	if !ok {
		return AutoBookConfig{}, fmt.Errorf("unknown mode %q (must be one of: weekday, weekend)", cfg.Mode)
	}
	if !sawAllowedWeekdays {
		cfg.Booking.AllowedWeekdays = append([]time.Weekday(nil), profile.AllowedWeekdays...)
	}
	if !sawAllowedDurations && !sawDurationMinutes {
		cfg.Booking.AllowedDurations = append([]int(nil), profile.AllowedDurations...)
	}
	if !sawStartWindow {
		cfg.Booking.StartWindow.From = profile.StartWindowFrom
		cfg.Booking.StartWindow.To = profile.StartWindowTo
	}
	if !sawMode {
		cfg.Mode = profile.Name
	}

	if err := validateAutoBookConfig(cfg); err != nil {
		return AutoBookConfig{}, err
	}
	return cfg, nil
}

func stripYAMLComment(input string) string {
	inSingle := false
	inDouble := false
	escaped := false
	for i, r := range input {
		switch r {
		case '\\':
			escaped = !escaped
			continue
		case '\'':
			if !inDouble && !escaped {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle && !escaped {
				inDouble = !inDouble
			}
		case '#':
			if !inSingle && !inDouble {
				return strings.TrimRight(input[:i], " ")
			}
		}
		escaped = false
	}
	return input
}

func countLeadingSpaces(input string) int {
	count := 0
	for _, r := range input {
		if r != ' ' {
			break
		}
		count++
	}
	return count
}

func parseYAMLScalar(input string) any {
	input = strings.TrimSpace(input)
	if strings.HasPrefix(input, "[") && strings.HasSuffix(input, "]") {
		inner := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(input, "["), "]"))
		if inner == "" {
			return []string{}
		}
		parts := strings.Split(inner, ",")
		values := make([]string, 0, len(parts))
		for _, part := range parts {
			values = append(values, unquoteYAML(strings.TrimSpace(part)))
		}
		return values
	}
	unquoted := unquoteYAML(input)
	switch strings.ToLower(unquoted) {
	case "true":
		return true
	case "false":
		return false
	}
	if n, err := strconv.Atoi(unquoted); err == nil {
		return n
	}
	return unquoted
}

func unquoteYAML(input string) string {
	if len(input) >= 2 {
		if (input[0] == '"' && input[len(input)-1] == '"') || (input[0] == '\'' && input[len(input)-1] == '\'') {
			return input[1 : len(input)-1]
		}
	}
	return input
}

func setAutoBookConfigValue(cfg *AutoBookConfig, path []string, value any) error {
	key := strings.Join(path, ".")
	switch key {
	case "mode":
		cfg.Mode = strings.ToLower(stringValue(value))
	case "dry_run":
		v, ok := value.(bool)
		if !ok {
			return fmt.Errorf("dry_run must be true or false")
		}
		cfg.DryRun = v
	case "timezone":
		cfg.Timezone = stringValue(value)
	case "venue.id":
		cfg.Venue.ID = stringValue(value)
	case "venue.name_exact":
		cfg.Venue.NameExact = stringValue(value)
	case "release.days_in_advance":
		cfg.Release.DaysInAdvance = intValue(value)
	case "release.time":
		cfg.Release.Time = stringValue(value)
	case "release.retry_until":
		cfg.Release.RetryUntil = stringValue(value)
	case "booking.allowed_weekdays":
		weekdays, err := weekdayValues(value)
		if err != nil {
			return err
		}
		cfg.Booking.AllowedWeekdays = weekdays
	case "booking.duration_minutes":
		// Back-compat with the original singular field — treated as a single-element allowed list.
		cfg.Booking.AllowedDurations = []int{intValue(value)}
	case "booking.allowed_durations":
		durations, err := intListValue(value)
		if err != nil {
			return err
		}
		cfg.Booking.AllowedDurations = durations
	case "booking.start_window.from":
		cfg.Booking.StartWindow.From = stringValue(value)
	case "booking.start_window.to":
		cfg.Booking.StartWindow.To = stringValue(value)
	case "booking.max_bookings_per_week":
		cfg.Booking.MaxBookingsPerWeek = intValue(value)
	case "booking.max_bookings_per_day":
		cfg.Booking.MaxBookingsPerDay = intValue(value)
	case "booking.players":
		cfg.Booking.Players = intValue(value)
	case "payment.method":
		cfg.Payment.Method = strings.ToUpper(stringValue(value))
	case "calendar.ical_url":
		cfg.Calendar.ICalURL = stringValue(value)
	case "notifications.telegram.enabled":
		v, ok := value.(bool)
		if !ok {
			return fmt.Errorf("notifications.telegram.enabled must be true or false")
		}
		cfg.Notifications.Telegram.Enabled = v
	case "notifications.telegram.bot_token_env":
		cfg.Notifications.Telegram.BotTokenEnv = stringValue(value)
	case "notifications.telegram.chat_id_env":
		cfg.Notifications.Telegram.ChatIDEnv = stringValue(value)
	case "notifications.whatsapp.enabled":
		v, ok := value.(bool)
		if !ok {
			return fmt.Errorf("notifications.whatsapp.enabled must be true or false")
		}
		cfg.Notifications.WhatsApp.Enabled = v
	case "polling.min_interval_seconds":
		cfg.Polling.MinIntervalSeconds = intValue(value)
	case "polling.max_interval_seconds":
		cfg.Polling.MaxIntervalSeconds = intValue(value)
	default:
		return fmt.Errorf("unknown config key %q", key)
	}
	return nil
}

func stringValue(value any) string {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case int:
		return strconv.Itoa(v)
	case bool:
		if v {
			return "true"
		}
		return "false"
	default:
		return ""
	}
}

func intValue(value any) int {
	switch v := value.(type) {
	case int:
		return v
	case string:
		n, _ := strconv.Atoi(strings.TrimSpace(v))
		return n
	default:
		return 0
	}
}

func intListValue(value any) ([]int, error) {
	var raw []string
	switch v := value.(type) {
	case []string:
		raw = v
	case string:
		for _, part := range strings.Split(v, ",") {
			raw = append(raw, strings.TrimSpace(part))
		}
	case int:
		return []int{v}, nil
	default:
		return nil, fmt.Errorf("expected list of integers")
	}
	out := make([]int, 0, len(raw))
	for _, entry := range raw {
		n, err := strconv.Atoi(strings.TrimSpace(entry))
		if err != nil {
			return nil, fmt.Errorf("invalid integer %q", entry)
		}
		out = append(out, n)
	}
	return out, nil
}

func weekdayValues(value any) ([]time.Weekday, error) {
	var labels []string
	switch v := value.(type) {
	case []string:
		labels = v
	case string:
		for _, part := range strings.Split(v, ",") {
			labels = append(labels, strings.TrimSpace(part))
		}
	default:
		return nil, fmt.Errorf("booking.allowed_weekdays must be a list")
	}

	weekdays := make([]time.Weekday, 0, len(labels))
	for _, label := range labels {
		weekday, ok := parseWeekday(label)
		if !ok {
			return nil, fmt.Errorf("invalid weekday %q", label)
		}
		weekdays = append(weekdays, weekday)
	}
	return weekdays, nil
}

func parseWeekday(input string) (time.Weekday, bool) {
	switch strings.ToLower(strings.TrimSpace(input)) {
	case "monday", "mon":
		return time.Monday, true
	case "tuesday", "tue", "tues":
		return time.Tuesday, true
	case "wednesday", "wed":
		return time.Wednesday, true
	case "thursday", "thu", "thur", "thurs":
		return time.Thursday, true
	case "friday", "fri":
		return time.Friday, true
	case "saturday", "sat":
		return time.Saturday, true
	case "sunday", "sun":
		return time.Sunday, true
	default:
		return time.Sunday, false
	}
}

func validateAutoBookConfig(cfg AutoBookConfig) error {
	profile, ok := autoBookProfiles[strings.ToLower(strings.TrimSpace(cfg.Mode))]
	if !ok {
		return fmt.Errorf("mode must be one of: weekday, weekend (got %q)", cfg.Mode)
	}
	if cfg.Timezone != autoBookTimezone {
		return fmt.Errorf("timezone must be %s", autoBookTimezone)
	}
	if _, err := time.LoadLocation(cfg.Timezone); err != nil {
		return fmt.Errorf("load timezone %q: %w", cfg.Timezone, err)
	}
	if strings.TrimSpace(cfg.Venue.ID) == "" {
		return fmt.Errorf("venue.id is required")
	}
	if normalizeAutoBookVenueName(cfg.Venue.NameExact) != normalizeAutoBookVenueName(autoBookVenueName) {
		return fmt.Errorf("venue.name_exact must be %q", autoBookVenueName)
	}
	if cfg.Release.DaysInAdvance != autoBookDaysInAdvance {
		return fmt.Errorf("release.days_in_advance must be %d", autoBookDaysInAdvance)
	}
	if cfg.Release.Time != autoBookReleaseTime {
		return fmt.Errorf("release.time must be %s", autoBookReleaseTime)
	}
	if cfg.Release.RetryUntil != autoBookRetryUntil {
		return fmt.Errorf("release.retry_until must be %s", autoBookRetryUntil)
	}
	if len(cfg.Booking.AllowedDurations) == 0 {
		return fmt.Errorf("booking.allowed_durations must not be empty")
	}
	for _, duration := range cfg.Booking.AllowedDurations {
		if !containsInt(profile.AllowedDurations, duration) {
			return fmt.Errorf("booking.allowed_durations may only include values from %v for mode %s", profile.AllowedDurations, profile.Name)
		}
	}
	if cfg.Booking.Players <= 0 {
		return fmt.Errorf("booking.players must be greater than 0")
	}
	if cfg.Booking.StartWindow.From != profile.StartWindowFrom || cfg.Booking.StartWindow.To != profile.StartWindowTo {
		return fmt.Errorf("booking.start_window must be %s to %s for mode %s", profile.StartWindowFrom, profile.StartWindowTo, profile.Name)
	}
	if cfg.Booking.MaxBookingsPerWeek <= 0 || cfg.Booking.MaxBookingsPerWeek > autoBookWeeklyCap {
		return fmt.Errorf("booking.max_bookings_per_week must be between 1 and %d", autoBookWeeklyCap)
	}
	if cfg.Booking.MaxBookingsPerDay <= 0 || cfg.Booking.MaxBookingsPerDay > autoBookDailyCap {
		return fmt.Errorf("booking.max_bookings_per_day must be %d", autoBookDailyCap)
	}
	if len(cfg.Booking.AllowedWeekdays) == 0 {
		return fmt.Errorf("booking.allowed_weekdays must not be empty")
	}
	for _, weekday := range cfg.Booking.AllowedWeekdays {
		if !weekdayInProfile(weekday, profile.AllowedWeekdays) {
			return fmt.Errorf("booking.allowed_weekdays may only include %s for mode %s", weekdayNames(profile.AllowedWeekdays), profile.Name)
		}
	}
	if cfg.Payment.Method == "" {
		return fmt.Errorf("payment.method is required")
	}
	if cfg.Notifications.WhatsApp.Enabled {
		return fmt.Errorf("WhatsApp notifications are not implemented yet")
	}
	if cfg.Polling.MinIntervalSeconds < 15 {
		return fmt.Errorf("polling.min_interval_seconds must be at least 15")
	}
	if cfg.Polling.MaxIntervalSeconds > 30 {
		return fmt.Errorf("polling.max_interval_seconds must be at most 30")
	}
	if cfg.Polling.MaxIntervalSeconds < cfg.Polling.MinIntervalSeconds {
		return fmt.Errorf("polling.max_interval_seconds must be greater than or equal to min_interval_seconds")
	}
	return nil
}

func weekdayInProfile(weekday time.Weekday, allowed []time.Weekday) bool {
	for _, candidate := range allowed {
		if candidate == weekday {
			return true
		}
	}
	return false
}

func weekdayNames(weekdays []time.Weekday) string {
	parts := make([]string, 0, len(weekdays))
	for _, weekday := range weekdays {
		parts = append(parts, weekday.String())
	}
	return strings.Join(parts, ", ")
}

func containsInt(haystack []int, needle int) bool {
	for _, entry := range haystack {
		if entry == needle {
			return true
		}
	}
	return false
}

func indexOfInt(haystack []int, needle int) int {
	for i, entry := range haystack {
		if entry == needle {
			return i
		}
	}
	return -1
}

func normalizeAutoBookVenueName(input string) string {
	return strings.ToLower(strings.Join(strings.Fields(input), " "))
}
