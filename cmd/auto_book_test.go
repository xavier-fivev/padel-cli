package cmd

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"padel-cli/storage"
)

func TestAutoBookTargetDateUsesSydneyDate(t *testing.T) {
	loc := mustLocation(t, autoBookTimezone)
	nowUTC := time.Date(2026, 5, 20, 23, 30, 0, 0, time.UTC)
	got := autoBookTargetDate(nowUTC, loc, autoBookDaysInAdvance)
	want := "2026-06-04"
	if got.Format("2006-01-02") != want {
		t.Fatalf("target date = %s, want %s", got.Format("2006-01-02"), want)
	}
}

func TestAutoBookWeekdayFilteringMondayThroughThursday(t *testing.T) {
	cfg := defaultAutoBookConfig()
	loc := mustLocation(t, autoBookTimezone)
	cases := []struct {
		date string
		want bool
	}{
		{"2026-06-01", true},
		{"2026-06-02", true},
		{"2026-06-03", true},
		{"2026-06-04", true},
		{"2026-06-05", false},
		{"2026-06-06", false},
		{"2026-06-07", false},
	}
	for _, tc := range cases {
		parsed, err := time.ParseInLocation("2006-01-02", tc.date, loc)
		if err != nil {
			t.Fatal(err)
		}
		got := isAllowedAutoBookWeekday(parsed, cfg.Booking.AllowedWeekdays)
		if got != tc.want {
			t.Fatalf("%s allowed = %v, want %v", tc.date, got, tc.want)
		}
	}
}

func TestFilterAutoBookCandidatesStartWindowInclusive(t *testing.T) {
	cfg := defaultAutoBookConfig()
	loc := mustLocation(t, autoBookTimezone)
	slots := []AvailabilitySlot{
		{Court: "Court 1", Time: "18:00", Duration: 90},
		{Court: "Court 1", Time: "18:30", Duration: 90},
		{Court: "Court 2", Time: "20:00", Duration: 90},
		{Court: "Court 2", Time: "20:30", Duration: 90},
	}
	got := filterAutoBookCandidates(slots, cfg, "2026-06-03", loc, nil)
	if len(got) != 2 {
		t.Fatalf("eligible slots = %d, want 2: %#v", len(got), got)
	}
	if got[0].Time != "18:30" || got[1].Time != "20:00" {
		t.Fatalf("eligible times = %s, %s; want 18:30, 20:00", got[0].Time, got[1].Time)
	}
}

func TestFilterAutoBookCandidatesRequiresNinetyMinutes(t *testing.T) {
	cfg := defaultAutoBookConfig()
	loc := mustLocation(t, autoBookTimezone)
	slots := []AvailabilitySlot{
		{Court: "Court 1", Time: "18:30", Duration: 60},
		{Court: "Court 2", Time: "19:00", Duration: 90},
	}
	got := filterAutoBookCandidates(slots, cfg, "2026-06-03", loc, nil)
	if len(got) != 1 {
		t.Fatalf("eligible slots = %d, want 1: %#v", len(got), got)
	}
	if got[0].Duration != 90 || got[0].Time != "19:00" {
		t.Fatalf("eligible slot = %#v, want 19:00 for 90 minutes", got[0])
	}
}

func TestWeeklyCapCounting(t *testing.T) {
	loc := mustLocation(t, autoBookTimezone)
	target, err := time.ParseInLocation("2006-01-02", "2026-06-04", loc)
	if err != nil {
		t.Fatal(err)
	}
	weekStart, weekEnd := bookingWeekBounds(target, loc)
	bookings := []storage.Booking{
		{Date: "2026-06-01", Time: "18:30"},
		{Date: "2026-06-03", Time: "20:00"},
		{Date: "2026-06-08", Time: "18:30"},
	}
	got := countBookingsInDateRange(bookings, weekStart, weekEnd)
	if got != 2 {
		t.Fatalf("weekly count = %d, want 2", got)
	}
}

func TestICalendarConflictDetection(t *testing.T) {
	loc := mustLocation(t, autoBookTimezone)
	ics := `BEGIN:VCALENDAR
BEGIN:VEVENT
DTSTART;TZID=Australia/Sydney:20260603T184500
DTEND;TZID=Australia/Sydney:20260603T191500
SUMMARY:Busy
END:VEVENT
END:VCALENDAR`
	events, err := parseICalendar(ics, loc)
	if err != nil {
		t.Fatal(err)
	}
	start, err := time.ParseInLocation("2006-01-02 15:04", "2026-06-03 18:30", loc)
	if err != nil {
		t.Fatal(err)
	}
	if !calendarHasConflict(events, start, start.Add(90*time.Minute)) {
		t.Fatal("expected calendar conflict")
	}
	later, err := time.ParseInLocation("2006-01-02 15:04", "2026-06-03 20:00", loc)
	if err != nil {
		t.Fatal(err)
	}
	if calendarHasConflict(events, later, later.Add(90*time.Minute)) {
		t.Fatal("did not expect calendar conflict")
	}
}

func TestICalendarWeeklyRecurrenceConflictDetection(t *testing.T) {
	loc := mustLocation(t, autoBookTimezone)
	ics := `BEGIN:VCALENDAR
BEGIN:VEVENT
DTSTART;TZID=Australia/Sydney:20260527T184500
DTEND;TZID=Australia/Sydney:20260527T191500
RRULE:FREQ=WEEKLY;COUNT=4;BYDAY=WE
SUMMARY:Weekly busy
END:VEVENT
END:VCALENDAR`
	events, err := parseICalendar(ics, loc)
	if err != nil {
		t.Fatal(err)
	}
	start, err := time.ParseInLocation("2006-01-02 15:04", "2026-06-03 18:30", loc)
	if err != nil {
		t.Fatal(err)
	}
	if !calendarHasConflict(events, start, start.Add(90*time.Minute)) {
		t.Fatal("expected recurring calendar conflict")
	}
}

func TestDryRunPreventsBookingExecution(t *testing.T) {
	called := false
	executed, err := executeUnlessDryRun(context.Background(), true, func(ctx context.Context) error {
		called = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if executed {
		t.Fatal("dry-run reported execution")
	}
	if called {
		t.Fatal("dry-run called booking executor")
	}
}

func TestLoadAutoBookConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	config := `dry_run: true
timezone: Australia/Sydney
venue:
  id: "tenant_123"
  name_exact: "Indoor Padel Australia Alexandria"
release:
  days_in_advance: 14
  time: "18:30"
  retry_until: "18:35"
booking:
  allowed_weekdays: [Monday, Tuesday, Wednesday, Thursday]
  duration_minutes: 90
  start_window:
    from: "18:30"
    to: "20:00"
  max_bookings_per_week: 2
  max_bookings_per_day: 1
  players: 4
payment:
  method: "MERCHANT_WALLET"
calendar:
  ical_url: ""
notifications:
  telegram:
    enabled: false
  whatsapp:
    enabled: false
polling:
  min_interval_seconds: 15
  max_interval_seconds: 30
`
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadAutoBookConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.DryRun {
		t.Fatal("dry_run = false, want true")
	}
	if cfg.Venue.ID != "tenant_123" {
		t.Fatalf("venue id = %q, want tenant_123", cfg.Venue.ID)
	}
	if cfg.Payment.Method != "MERCHANT_WALLET" {
		t.Fatalf("payment method = %q, want MERCHANT_WALLET", cfg.Payment.Method)
	}
}

func mustLocation(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}
