package server

import (
	"net/http"
	"time"

	"padel-cli/storage"
)

type dashboardRow struct {
	storage.Booking
	StartLocal       time.Time
	CancelDeadline   time.Time
	CancelDeadlineTZ string
	UrgencyClass     string // "badge-red" | "badge-amber" | "badge-muted"
	UrgencyLabel     string // e.g. "Cancel in 18h"
}

type dashboardData struct {
	Title  string
	Active string
	Rows   []dashboardRow
	Now    time.Time
	Wallet walletStatus
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	now := time.Now()
	bookings, err := storage.ListBookings(s.db, storage.BookingFilter{
		Upcoming: true,
		NowDate:  now.Format("2006-01-02"),
		NowTime:  now.Format("15:04"),
	})
	if err != nil {
		s.logger.Printf("dashboard list bookings: %v", err)
		http.Error(w, "failed to load bookings", http.StatusInternalServerError)
		return
	}

	rows := make([]dashboardRow, 0, len(bookings))
	for _, booking := range bookings {
		row := dashboardRow{Booking: booking}
		loc := venueLoc(booking.VenueTimezone)
		if booking.StartUTC != "" {
			if parsed, err := time.Parse(time.RFC3339, booking.StartUTC); err == nil {
				row.StartLocal = parsed.In(loc)
				row.CancelDeadline = parsed.Add(-48 * time.Hour).In(loc)
				row.CancelDeadlineTZ = booking.VenueTimezone
				row.UrgencyClass, row.UrgencyLabel = cancelUrgency(row.CancelDeadline, now)
			}
		}
		rows = append(rows, row)
	}

	s.render(w, "dashboard", dashboardData{
		Title:  "Dashboard",
		Active: "dashboard",
		Rows:   rows,
		Now:    now,
		Wallet: s.cachedWalletStatus(),
	})
}

func cancelUrgency(deadline, now time.Time) (string, string) {
	remaining := deadline.Sub(now)
	if remaining <= 0 {
		return "badge-muted", "Cancel deadline passed"
	}
	hours := int(remaining.Hours())
	switch {
	case remaining < 24*time.Hour:
		return "badge-red", formatRemaining(remaining)
	case remaining < 72*time.Hour:
		return "badge-amber", formatRemaining(remaining)
	default:
		_ = hours
		return "badge-muted", deadline.Format("Mon 2 Jan 15:04")
	}
}

func formatRemaining(d time.Duration) string {
	if d <= 0 {
		return "passed"
	}
	hours := int(d.Hours())
	if hours >= 24 {
		days := hours / 24
		if days == 1 {
			return "1 day left"
		}
		return formatDays(days)
	}
	if hours >= 1 {
		if hours == 1 {
			return "1h left"
		}
		return formatHours(hours)
	}
	mins := int(d.Minutes())
	return formatMinutes(mins)
}

func formatDays(d int) string  { return itoa(d) + "d left" }
func formatHours(h int) string { return itoa(h) + "h left" }
func formatMinutes(m int) string {
	if m <= 0 {
		return "<1m left"
	}
	return itoa(m) + "m left"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	buf := [20]byte{}
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func venueLoc(tz string) *time.Location {
	if tz == "" {
		return time.UTC
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return time.UTC
	}
	return loc
}
