package cmd

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"time"

	"padel-cli/api"
	"padel-cli/storage"

	"github.com/spf13/cobra"
)

type autoBookRunOptions struct {
	IgnoreReleaseWindow bool
	Now                 func() time.Time
	Sleep               func(time.Duration)
}

type autoBookAudit struct {
	db         *sql.DB
	runID      string
	targetDate string
	venueID    string
}

func autoBookCmd() *cobra.Command {
	var configPath string
	var ignoreReleaseWindow bool

	cmd := &cobra.Command{
		Use:   "auto-book",
		Short: "Autonomously book a pre-authorised Playtomic court",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadAutoBookConfig(configPath)
			if err != nil {
				return err
			}
			return runAutoBook(cmd.Context(), cfg, autoBookRunOptions{
				IgnoreReleaseWindow: ignoreReleaseWindow,
				Now:                 time.Now,
				Sleep:               time.Sleep,
			})
		},
	}

	cmd.Flags().StringVar(&configPath, "config", "", "Auto-book YAML config")
	cmd.Flags().BoolVar(&ignoreReleaseWindow, "ignore-release-window", false, "Allow a dry-run outside 18:30-18:35 for local testing")
	return cmd
}

func runAutoBook(ctx context.Context, cfg AutoBookConfig, opts autoBookRunOptions) error {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Sleep == nil {
		opts.Sleep = time.Sleep
	}
	// --ignore-release-window is now allowed even when dry_run is false. The
	// release window is the autonomous schedule signal — when the bot fires
	// itself at 18:30 Sydney it must respect it. But a manual operator who
	// passes the flag is opportunistically searching for slots that came back
	// onto the market via cancellations between release windows. All other
	// safety guards (72h lead time, caps, venue verify, payment-challenge
	// abort, forbidden-publish test) still apply.

	loc, err := time.LoadLocation(cfg.Timezone)
	if err != nil {
		return err
	}
	time.Local = loc

	db, err := storage.OpenBookingsDB()
	if err != nil {
		return err
	}
	defer db.Close()

	now := opts.Now().In(loc)
	targetDate := autoBookTargetDate(now, loc, cfg.Release.DaysInAdvance)
	targetDateStr := targetDate.Format("2006-01-02")
	audit := autoBookAudit{
		db:         db,
		runID:      newBookingID(),
		targetDate: targetDateStr,
		venueID:    cfg.Venue.ID,
	}

	notifier, notifierErr := newNotifier(cfg.Notifications)
	if notifierErr != nil {
		audit.log("error", "notification_setup_failed", notifierErr.Error(), "", nil)
		return notifierErr
	}

	audit.log("info", "run_started", fmt.Sprintf("target date %s from local date %s", targetDateStr, now.Format("2006-01-02")), "", map[string]any{
		"dry_run": cfg.DryRun,
	})

	if !isAllowedAutoBookWeekday(targetDate, cfg.Booking.AllowedWeekdays) {
		audit.log("info", "skipped_weekday", fmt.Sprintf("%s is %s, outside Monday-Thursday rule", targetDateStr, targetDate.Weekday()), "", nil)
		return nil
	}

	if !opts.IgnoreReleaseWindow {
		nowAfter, decision := waitForReleaseWindow(now, cfg, loc, opts.Sleep, opts.Now, audit)
		now = nowAfter
		if decision == waitDecisionSkip {
			return nil
		}
	} else {
		audit.log("info", "release_window_bypass", "manual run — searching for any available slot regardless of release window", "", nil)
	}

	creds, err := loadAutoBookCredentials(ctx)
	if err != nil {
		return stopAndNotify(ctx, notifier, audit, "auth_failed", "Auto-book stopped: Playtomic authentication is not ready", err)
	}

	tenant, venueTZ, resources, err := loadAndVerifyAutoBookVenue(ctx, cfg)
	if err != nil {
		return stopAndNotify(ctx, notifier, audit, "venue_verification_failed", "Auto-book stopped: configured venue could not be verified", err)
	}
	audit.log("info", "venue_verified", fmt.Sprintf("verified venue %s (%s)", tenant.TenantName, tenant.TenantID), "", map[string]any{
		"timezone": venueTZ,
	})

	if _, _, err := syncBookingsForAutoBook(ctx, db, cfg, creds, 100); err != nil {
		return stopAndNotify(ctx, notifier, audit, "booking_sync_failed", "Auto-book stopped: could not sync existing Playtomic bookings", err)
	}

	if err := enforceBookingCaps(db, cfg, targetDate, loc, audit); err != nil {
		audit.log("info", "skipped_booking_cap", err.Error(), "", nil)
		return nil
	}

	calendarEvents, err := fetchICalendar(ctx, cfg.Calendar.ICalURL, loc)
	if err != nil {
		return stopAndNotify(ctx, notifier, audit, "calendar_failed", "Auto-book stopped: iCalendar conflict check failed", err)
	}
	audit.log("info", "calendar_checked", fmt.Sprintf("loaded %d calendar events", len(calendarEvents)), "", nil)

	deadline := releaseWindowEnd(now, cfg, loc)
	attempt := 0
	for {
		attempt++
		candidate, candidates, err := findAutoBookCandidate(ctx, cfg, tenant, venueTZ, resources, targetDate, calendarEvents, opts.Now().In(loc))
		if err != nil {
			return stopAndNotify(ctx, notifier, audit, "availability_failed", "Auto-book stopped: availability lookup failed", err)
		}
		audit.log("info", "availability_checked", fmt.Sprintf("attempt %d found %d eligible slots", attempt, len(candidates)), "", map[string]any{
			"attempt": attempt,
		})

		if candidate != nil {
			slot := *candidate
			audit.log("info", "candidate_selected", fmt.Sprintf("selected %s %dmin on %s", slot.Time, slot.Duration, slot.Court), slot.Time, map[string]any{
				"court":       slot.Court,
				"resource_id": slot.ResourceID,
				"duration":    slot.Duration,
				"price":       slot.Price,
			})
			slotStart, _, _ := availabilitySlotInterval(slot, targetDateStr, loc, slot.Duration)
			cancelDeadline := slotStart.Add(-autoBookFreeCancelMargin)
			cancelDeadlineLabel := cancelDeadline.Format("Mon 2 Jan 15:04 MST")
			executed, err := executeUnlessDryRun(ctx, cfg.DryRun, func(ctx context.Context) error {
				audit.log("info", "booking_attempt_started", fmt.Sprintf("booking %s %s %dmin", targetDateStr, slot.Time, slot.Duration), slot.Time, nil)
				booking, err := executeAutoBookBooking(ctx, cfg, creds, tenant, slot, targetDateStr, venueTZ)
				if err != nil {
					return err
				}
				if _, err := storage.AddBookingIfNotExists(db, booking); err != nil {
					return fmt.Errorf("store confirmed booking: %w", err)
				}
				audit.log("info", "booking_confirmed", fmt.Sprintf("booked %s %s on %s; free-cancel until %s", tenant.TenantName, slot.Time, targetDateStr, cancelDeadlineLabel), slot.Time, map[string]any{
					"booking_id":             booking.ID,
					"cancel_deadline_local":  cancelDeadlineLabel,
					"cancel_deadline_utc":    cancelDeadline.UTC().Format(time.RFC3339),
				})
				notifyBestEffort(ctx, notifier, audit, fmt.Sprintf("Padel booked: %s %s at %s (%s, %d min). Free-cancel until %s — match stays private; invite players or cancel before then.", targetDateStr, slot.Time, tenant.TenantName, slot.Court, slot.Duration, cancelDeadlineLabel))
				return nil
			})
			if err != nil {
				return stopAndNotify(ctx, notifier, audit, "booking_failed", "Auto-book stopped: checkout did not complete safely", err)
			}
			if !executed {
				audit.log("info", "dry_run_booking_prevented", fmt.Sprintf("dry-run would book %s %s on %s; free-cancel would be until %s", tenant.TenantName, slot.Time, targetDateStr, cancelDeadlineLabel), slot.Time, map[string]any{
					"cancel_deadline_local": cancelDeadlineLabel,
				})
				notifyBestEffort(ctx, notifier, audit, fmt.Sprintf("Padel dry-run: would book %s %s at %s (%s, %d min). Cancel deadline would be %s.", targetDateStr, slot.Time, tenant.TenantName, slot.Court, slot.Duration, cancelDeadlineLabel))
			}
			return nil
		}

		now = opts.Now().In(loc)
		if opts.IgnoreReleaseWindow || !now.Before(deadline) {
			audit.log("info", "no_slot_before_deadline", "no eligible slot found before retry deadline", "", nil)
			return nil
		}
		delay := conservativePollingDelay(cfg, now, deadline)
		if delay <= 0 {
			audit.log("info", "no_slot_before_deadline", "retry deadline reached", "", nil)
			return nil
		}
		audit.log("info", "retry_wait", fmt.Sprintf("waiting %s before next availability check", delay), "", nil)
		opts.Sleep(delay)
	}
}

func (a autoBookAudit) log(level, decision, message, slotTime string, metadata map[string]any) {
	fmt.Printf("%s %-28s %s\n", strings.ToUpper(level), decision, message)
	if a.db == nil {
		return
	}
	if err := storage.AddAuditEvent(a.db, storage.AuditEvent{
		RunID:      a.runID,
		Level:      level,
		Decision:   decision,
		Message:    message,
		TargetDate: a.targetDate,
		SlotTime:   slotTime,
		VenueID:    a.venueID,
		Metadata:   metadata,
	}); err != nil {
		fmt.Printf("WARN audit_log_failed %v\n", err)
	}
}

func stopAndNotify(ctx context.Context, notifier Notifier, audit autoBookAudit, decision, message string, err error) error {
	fullMessage := message
	if err != nil {
		fullMessage = fmt.Sprintf("%s: %v", message, err)
	}
	audit.log("error", decision, fullMessage, "", nil)
	notifyBestEffort(ctx, notifier, audit, fullMessage)
	return fmt.Errorf("%s", fullMessage)
}

func notifyBestEffort(ctx context.Context, notifier Notifier, audit autoBookAudit, message string) {
	if notifier == nil {
		return
	}
	if err := notifier.Notify(ctx, message); err != nil {
		audit.log("warn", "notification_failed", err.Error(), "", nil)
	}
}

func autoBookTargetDate(now time.Time, loc *time.Location, daysInAdvance int) time.Time {
	local := now.In(loc)
	localDate := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	return localDate.AddDate(0, 0, daysInAdvance)
}

func isAllowedAutoBookWeekday(date time.Time, allowed []time.Weekday) bool {
	for _, weekday := range allowed {
		if date.Weekday() == weekday {
			return true
		}
	}
	return false
}

func withinReleaseWindow(now time.Time, cfg AutoBookConfig, loc *time.Location) bool {
	start := releaseWindowStart(now, cfg, loc)
	end := releaseWindowEnd(now, cfg, loc)
	return !now.Before(start) && !now.After(end)
}

func releaseWindowStart(now time.Time, cfg AutoBookConfig, loc *time.Location) time.Time {
	minutes, _ := parseClock(cfg.Release.Time)
	local := now.In(loc)
	return time.Date(local.Year(), local.Month(), local.Day(), minutes/60, minutes%60, 0, 0, loc)
}

func releaseWindowEnd(now time.Time, cfg AutoBookConfig, loc *time.Location) time.Time {
	minutes, _ := parseClock(cfg.Release.RetryUntil)
	local := now.In(loc)
	return time.Date(local.Year(), local.Month(), local.Day(), minutes/60, minutes%60, 0, 0, loc)
}

type waitDecision int

const (
	waitDecisionProceed waitDecision = iota
	waitDecisionSkip
)

// preWindowMaxAttempts caps how long we'll poll waiting for the release
// window to open. At 10s per attempt this is ~10 minutes — comfortably
// longer than any reasonable scheduler lag, but bounded so a misconfigured
// run can't loop forever.
const preWindowMaxAttempts = 60

func waitForReleaseWindow(now time.Time, cfg AutoBookConfig, loc *time.Location, sleep func(time.Duration), nowFn func() time.Time, audit autoBookAudit) (time.Time, waitDecision) {
	windowStart := releaseWindowStart(now, cfg, loc)
	windowEnd := releaseWindowEnd(now, cfg, loc)

	if now.After(windowEnd) {
		audit.log("info", "skipped_release_window", fmt.Sprintf("now %s is after window end %s %s", now.Format("15:04:05"), cfg.Release.RetryUntil, cfg.Timezone), "", nil)
		return now, waitDecisionSkip
	}

	attempts := 0
	for now.Before(windowStart) && attempts < preWindowMaxAttempts {
		audit.log("info", "waiting_for_release_window", fmt.Sprintf("now %s, window opens %s — sleeping 10s", now.Format("15:04:05"), windowStart.Format("15:04:05")), "", map[string]any{
			"attempt": attempts + 1,
		})
		sleep(10 * time.Second)
		now = nowFn().In(loc)
		attempts++
	}
	if now.Before(windowStart) {
		audit.log("info", "pre_window_wait_exceeded", fmt.Sprintf("window did not open within %d attempts; giving up", preWindowMaxAttempts), "", nil)
		return now, waitDecisionSkip
	}
	return now, waitDecisionProceed
}

func loadAutoBookCredentials(ctx context.Context) (*storage.Credentials, error) {
	creds, err := storage.LoadCredentials()
	if err != nil {
		return nil, err
	}
	if creds == nil || creds.AccessToken == "" {
		return nil, fmt.Errorf("not logged in. Run 'padel auth login' first")
	}
	if creds.AccessTokenExpired(time.Now()) {
		if creds.RefreshToken == "" {
			return nil, fmt.Errorf("token expired and no refresh token is available. Run 'padel auth login'")
		}
		refreshed, err := client.RefreshToken(ctx, creds.RefreshToken)
		if err != nil {
			return nil, fmt.Errorf("token refresh failed: %w. Run 'padel auth login'", err)
		}
		creds.AccessToken = refreshed.AccessToken
		creds.AccessTokenExpiration = refreshed.AccessTokenExpiration
		creds.RefreshToken = refreshed.RefreshToken
		creds.RefreshTokenExpiration = refreshed.RefreshTokenExpiration
		if err := storage.SaveCredentials(creds); err != nil {
			return nil, fmt.Errorf("save refreshed credentials: %w", err)
		}
	}
	client.AccessToken = creds.AccessToken
	return creds, nil
}

func loadAndVerifyAutoBookVenue(ctx context.Context, cfg AutoBookConfig) (api.Tenant, string, []api.Resource, error) {
	tenant, err := client.GetTenant(ctx, cfg.Venue.ID)
	if err != nil {
		return api.Tenant{}, "", nil, err
	}
	if normalizeAutoBookVenueName(tenant.TenantName) != normalizeAutoBookVenueName(cfg.Venue.NameExact) {
		return api.Tenant{}, "", nil, fmt.Errorf("configured venue id resolved to %q, expected %q", tenant.TenantName, cfg.Venue.NameExact)
	}

	venueTZ := tenant.Address.TimeZone
	if strings.TrimSpace(venueTZ) == "" {
		venueTZ = cfg.Timezone
	}
	if venueTZ != cfg.Timezone {
		return api.Tenant{}, "", nil, fmt.Errorf("venue timezone %q does not match required runtime timezone %q", venueTZ, cfg.Timezone)
	}
	if _, err := time.LoadLocation(venueTZ); err != nil {
		return api.Tenant{}, "", nil, fmt.Errorf("load venue timezone: %w", err)
	}

	resources, err := client.GetResources(ctx, cfg.Venue.ID)
	if err != nil {
		resources = tenant.Resources
	}
	return tenant, venueTZ, resources, nil
}

func syncBookingsForAutoBook(ctx context.Context, db *sql.DB, cfg AutoBookConfig, creds *storage.Credentials, size int) (int, int, error) {
	matches, err := client.GetMatches(ctx, size, "start_date,DESC", creds.UserID)
	if err != nil {
		return 0, 0, err
	}

	venues, err := storage.LoadVenues()
	if err != nil {
		return 0, 0, err
	}
	venueByID := map[string]storage.Venue{}
	for _, venue := range venues {
		venueByID[venue.ID] = venue
	}
	venueByID[cfg.Venue.ID] = storage.Venue{
		ID:       cfg.Venue.ID,
		Alias:    "auto-book",
		Name:     cfg.Venue.NameExact,
		Indoor:   true,
		TimeZone: cfg.Timezone,
	}

	added := 0
	skipped := 0
	for _, match := range matches {
		booking := bookingFromMatch(match, venueByID)
		inserted, err := storage.AddBookingIfNotExists(db, booking)
		if err != nil {
			return added, skipped, err
		}
		if inserted {
			added++
		} else {
			skipped++
		}
	}
	return added, skipped, nil
}

func bookingFromMatch(match api.Match, venueByID map[string]storage.Venue) storage.Booking {
	venueTZ := match.Tenant.Address.TimeZone
	if venue, ok := venueByID[match.Tenant.TenantID]; ok && venue.TimeZone != "" {
		venueTZ = venue.TimeZone
	}
	localDate, localTime, startUTC, _ := apiUTCToLocal(match.StartDate, venueTZ)
	if localDate == "" {
		localDate = dateFromMatch(match.StartDate)
	}
	if localTime == "" {
		localTime = timeFromMatch(match.StartDate)
	}

	booking := storage.Booking{
		ID:            match.MatchID,
		VenueName:     match.Tenant.TenantName,
		VenueID:       match.Tenant.TenantID,
		Court:         match.ResourceName,
		Date:          localDate,
		Time:          localTime,
		StartUTC:      startUTC,
		VenueTimezone: normalizeVenueTimezone(venueTZ),
		Duration:      durationFromMatch(match.StartDate, match.EndDate),
		Price:         parsePriceAmount(match.Price),
		BookedAt:      match.CreatedAt,
		Source:        "playtomic_sync",
	}
	if venue, ok := venueByID[booking.VenueID]; ok {
		booking.VenueAlias = venue.Alias
	}
	if booking.VenueName == "" {
		booking.VenueName = booking.VenueAlias
	}
	return booking
}

func enforceBookingCaps(db *sql.DB, cfg AutoBookConfig, targetDate time.Time, loc *time.Location, audit autoBookAudit) error {
	weekStart, weekEnd := bookingWeekBounds(targetDate, loc)
	bookings, err := storage.ListBookings(db, storage.BookingFilter{
		From: weekStart.Format("2006-01-02"),
		To:   weekEnd.Format("2006-01-02"),
	})
	if err != nil {
		return err
	}
	targetDateStr := targetDate.Format("2006-01-02")
	dayCount := countBookingsOnDate(bookings, targetDateStr)
	weekCount := countBookingsInDateRange(bookings, weekStart, weekEnd)
	audit.log("info", "caps_checked", fmt.Sprintf("week bookings %d/%d, day bookings %d/%d", weekCount, cfg.Booking.MaxBookingsPerWeek, dayCount, cfg.Booking.MaxBookingsPerDay), "", nil)

	if dayCount >= cfg.Booking.MaxBookingsPerDay {
		return fmt.Errorf("existing booking already exists for %s", targetDateStr)
	}
	if weekCount >= cfg.Booking.MaxBookingsPerWeek {
		return fmt.Errorf("weekly cap reached for week starting %s", weekStart.Format("2006-01-02"))
	}
	return nil
}

func bookingWeekBounds(date time.Time, loc *time.Location) (time.Time, time.Time) {
	local := date.In(loc)
	start := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	daysFromMonday := (int(start.Weekday()) + 6) % 7
	start = start.AddDate(0, 0, -daysFromMonday)
	end := start.AddDate(0, 0, 6)
	return start, end
}

func countBookingsOnDate(bookings []storage.Booking, date string) int {
	count := 0
	for _, booking := range bookings {
		if booking.Date == date {
			count++
		}
	}
	return count
}

func countBookingsInDateRange(bookings []storage.Booking, start, end time.Time) int {
	startDate := start.Format("2006-01-02")
	endDate := end.Format("2006-01-02")
	count := 0
	for _, booking := range bookings {
		if booking.Date >= startDate && booking.Date <= endDate {
			count++
		}
	}
	return count
}

// autoBookFreeCancelMargin is how long before play the free-cancel window
// closes on a private match. Playtomic policy at this venue is 48h.
const autoBookFreeCancelMargin = 48 * time.Hour

// minAutoBookLeadTime is a hard safety bound. We require 72h between now and
// the slot so there's always a clean 24h of margin above the free-cancel
// deadline. Today+14 (autoBookDaysInAdvance) is always well above this, but
// this check defends against future config drift.
const minAutoBookLeadTime = 72 * time.Hour

func findAutoBookCandidate(ctx context.Context, cfg AutoBookConfig, tenant api.Tenant, venueTZ string, resources []api.Resource, targetDate time.Time, calendarEvents []CalendarEvent, now time.Time) (*AvailabilitySlot, []AvailabilitySlot, error) {
	loc := venueLocation(venueTZ)
	startLocal := time.Date(targetDate.Year(), targetDate.Month(), targetDate.Day(), 0, 0, 0, 0, loc)
	endLocal := time.Date(targetDate.Year(), targetDate.Month(), targetDate.Day(), 23, 59, 59, 0, loc)
	availability, err := client.GetAvailability(ctx, tenant.TenantID, startLocal.UTC(), endLocal.UTC())
	if err != nil {
		return nil, nil, err
	}

	resourceInfo := map[string]api.Resource{}
	for _, resource := range resources {
		resourceInfo[resource.ResourceID] = resource
	}
	if len(resourceInfo) == 0 {
		for _, resource := range tenant.Resources {
			resourceInfo[resource.ResourceID] = resource
		}
	}

	startMinutes, _ := parseClock(cfg.Booking.StartWindow.From)
	endMinutes, _ := parseClock(cfg.Booking.StartWindow.To)
	targetDateStr := targetDate.Format("2006-01-02")
	slots := filterAvailabilityWithResources(availability, resourceInfo, startMinutes, endMinutes, true, targetDateStr, venueTZ, false, false)
	candidates := filterAutoBookCandidates(slots, cfg, targetDateStr, loc, calendarEvents, now)
	if len(candidates) == 0 {
		return nil, candidates, nil
	}
	return &candidates[0], candidates, nil
}

func filterAutoBookCandidates(slots []AvailabilitySlot, cfg AutoBookConfig, targetDate string, loc *time.Location, calendarEvents []CalendarEvent, now time.Time) []AvailabilitySlot {
	startMinutes, _ := parseClock(cfg.Booking.StartWindow.From)
	endMinutes, _ := parseClock(cfg.Booking.StartWindow.To)
	candidates := []AvailabilitySlot{}
	for _, slot := range slots {
		if !containsInt(cfg.Booking.AllowedDurations, slot.Duration) {
			continue
		}
		minutes, err := parseClock(slot.Time)
		if err != nil {
			continue
		}
		if minutes < startMinutes || minutes > endMinutes {
			continue
		}
		start, end, err := availabilitySlotInterval(slot, targetDate, loc, slot.Duration)
		if err != nil {
			continue
		}
		if start.Sub(now) < minAutoBookLeadTime {
			// Bookings must be at least 72h out so we always have a clean
			// 24h margin above the 48h free-cancel deadline.
			continue
		}
		if calendarHasConflict(calendarEvents, start, end) {
			continue
		}
		candidates = append(candidates, slot)
	}
	sort.Slice(candidates, func(i, j int) bool {
		// Prefer durations earlier in the allowed list (e.g. 90 before 120),
		// then earliest start time, then court name for stability.
		leftDurIdx := indexOfInt(cfg.Booking.AllowedDurations, candidates[i].Duration)
		rightDurIdx := indexOfInt(cfg.Booking.AllowedDurations, candidates[j].Duration)
		if leftDurIdx != rightDurIdx {
			return leftDurIdx < rightDurIdx
		}
		left, _ := parseClock(candidates[i].Time)
		right, _ := parseClock(candidates[j].Time)
		if left == right {
			return candidates[i].Court < candidates[j].Court
		}
		return left < right
	})
	return candidates
}

func availabilitySlotInterval(slot AvailabilitySlot, targetDate string, loc *time.Location, duration int) (time.Time, time.Time, error) {
	if slot.StartUTC != "" {
		parsed, err := time.Parse(time.RFC3339, slot.StartUTC)
		if err == nil {
			start := parsed.In(loc)
			return start, start.Add(time.Duration(duration) * time.Minute), nil
		}
	}
	start, err := time.ParseInLocation("2006-01-02 15:04", fmt.Sprintf("%s %s", targetDate, slot.Time), loc)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	return start, start.Add(time.Duration(duration) * time.Minute), nil
}

func executeUnlessDryRun(ctx context.Context, dryRun bool, exec func(context.Context) error) (bool, error) {
	if dryRun {
		return false, nil
	}
	return true, exec(ctx)
}

func executeAutoBookBooking(ctx context.Context, cfg AutoBookConfig, creds *storage.Credentials, tenant api.Tenant, slot AvailabilitySlot, targetDateStr, venueTZ string) (storage.Booking, error) {
	loc := venueLocation(venueTZ)
	start, _, err := availabilitySlotInterval(slot, targetDateStr, loc, slot.Duration)
	if err != nil {
		return storage.Booking{}, err
	}
	startUTC := start.UTC()

	intent := api.PaymentIntentRequest{
		AllowedPaymentMethodTypes: []string{cfg.Payment.Method},
		UserID:                    creds.UserID,
		Cart: api.PaymentIntentCart{
			RequestedItem: api.PaymentIntentItem{
				CartItemType:      "CUSTOMER_MATCH",
				CartItemVoucherID: nil,
				CartItemData: api.PaymentIntentItemData{
					SupportsSplitPayment: true,
					NumberOfPlayers:      cfg.Booking.Players,
					TenantID:             tenant.TenantID,
					ResourceID:           slot.ResourceID,
					Start:                startUTC.Format("2006-01-02T15:04:05"),
					Duration:             slot.Duration,
					MatchRegistrations: []api.MatchRegistration{
						{UserID: creds.UserID, PayNow: true},
					},
				},
			},
		},
	}

	intentResp, err := client.CreatePaymentIntent(ctx, intent)
	if err != nil {
		return storage.Booking{}, err
	}

	availableMethods := extractPaymentMethods(intentResp.AvailablePaymentMethods)
	selected, err := chooseRequiredPaymentMethod(availableMethods, cfg.Payment.Method)
	if err != nil {
		return storage.Booking{}, err
	}
	if err := client.UpdatePaymentIntent(ctx, intentResp.PaymentIntentID, api.PaymentIntentUpdateRequest{SelectedPaymentMethod: selected}); err != nil {
		return storage.Booking{}, err
	}

	confirmResp, err := client.ConfirmPaymentIntent(ctx, intentResp.PaymentIntentID)
	if err != nil {
		return storage.Booking{}, err
	}
	if err := unexpectedCheckoutState(confirmResp); err != nil {
		return storage.Booking{}, err
	}
	bookingID := extractBookingID(confirmResp)
	if bookingID == "" {
		return storage.Booking{}, fmt.Errorf("unexpected checkout state: confirmation response did not include a booking id")
	}

	return storage.Booking{
		ID:            bookingID,
		VenueAlias:    "auto-book",
		VenueName:     tenant.TenantName,
		VenueID:       tenant.TenantID,
		Court:         slot.Court,
		Date:          targetDateStr,
		Time:          slot.Time,
		StartUTC:      startUTC.Format(time.RFC3339),
		VenueTimezone: venueTZ,
		Duration:      slot.Duration,
		Price:         parsePriceAmount(slot.Price),
		BookedAt:      time.Now().UTC().Format(time.RFC3339),
		Source:        "auto_booked",
	}, nil
}

func chooseRequiredPaymentMethod(available []string, requested string) (string, error) {
	for _, method := range available {
		if strings.EqualFold(method, requested) {
			return method, nil
		}
	}
	if len(available) == 0 {
		return "", fmt.Errorf("payment intent did not return available payment methods; refusing autonomous checkout")
	}
	return "", fmt.Errorf("payment method %q not available. Available: %s", requested, strings.Join(available, ", "))
}

func unexpectedCheckoutState(payload map[string]any) error {
	for _, key := range []string{"next_action", "payment_challenge", "authentication_url", "redirect_url"} {
		if value, ok := payload[key]; ok && value != nil {
			return fmt.Errorf("unexpected checkout state: confirmation returned %s", key)
		}
	}
	for _, key := range []string{"status", "state", "payment_status"} {
		raw, ok := payload[key]
		if !ok {
			continue
		}
		value := strings.ToLower(fmt.Sprint(raw))
		if strings.Contains(value, "requires") ||
			strings.Contains(value, "challenge") ||
			strings.Contains(value, "captcha") ||
			strings.Contains(value, "mfa") ||
			strings.Contains(value, "3ds") ||
			strings.Contains(value, "pending") {
			return fmt.Errorf("unexpected checkout state: %s=%s", key, value)
		}
	}
	return nil
}

func conservativePollingDelay(cfg AutoBookConfig, now, deadline time.Time) time.Duration {
	minSeconds := cfg.Polling.MinIntervalSeconds
	maxSeconds := cfg.Polling.MaxIntervalSeconds
	if maxSeconds < minSeconds {
		maxSeconds = minSeconds
	}
	seconds := minSeconds
	if maxSeconds > minSeconds {
		source := rand.New(rand.NewSource(time.Now().UnixNano()))
		seconds += source.Intn(maxSeconds - minSeconds + 1)
	}
	delay := time.Duration(seconds) * time.Second
	remaining := deadline.Sub(now)
	if delay > remaining {
		delay = remaining
	}
	return delay
}
