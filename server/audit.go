package server

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

type auditEvent struct {
	ID         string
	RunID      string
	Time       time.Time
	Level      string
	Decision   string
	Message    string
	TargetDate string
	SlotTime   string
	VenueID    string
	Metadata   string // raw json, displayed as-is
}

type auditRunGroup struct {
	RunID      string
	FirstTime  time.Time
	LastTime   time.Time
	EventCount int
	HasError   bool
	Events     []auditEvent
}

type auditData struct {
	Title      string
	Active     string
	Runs       []auditRunGroup
	LevelFilter string
	RunFilter   string
	Levels     []string
}

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	levelFilter := strings.TrimSpace(r.URL.Query().Get("level"))
	runFilter := strings.TrimSpace(r.URL.Query().Get("run"))

	conds := []string{}
	args := []any{}
	if levelFilter != "" {
		conds = append(conds, "level = ?")
		args = append(args, levelFilter)
	}
	if runFilter != "" {
		conds = append(conds, "run_id = ?")
		args = append(args, runFilter)
	}

	query := `SELECT id, run_id, event_time_utc, level, decision, message, target_date, slot_time, venue_id, metadata_json FROM auto_book_audit`
	if len(conds) > 0 {
		query += " WHERE " + strings.Join(conds, " AND ")
	}
	query += " ORDER BY event_time_utc DESC LIMIT 500"

	rows, err := s.db.Query(query, args...)
	if err != nil {
		s.logger.Printf("audit query: %v", err)
		http.Error(w, "audit query failed", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	groups := map[string]*auditRunGroup{}
	order := []string{}
	for rows.Next() {
		var ev auditEvent
		var runID, metaJSON sql.NullString
		var eventTime string
		if err := rows.Scan(&ev.ID, &runID, &eventTime, &ev.Level, &ev.Decision, &ev.Message, &ev.TargetDate, &ev.SlotTime, &ev.VenueID, &metaJSON); err != nil {
			s.logger.Printf("audit scan: %v", err)
			continue
		}
		if runID.Valid {
			ev.RunID = runID.String
		}
		if metaJSON.Valid && metaJSON.String != "" && metaJSON.String != "{}" {
			ev.Metadata = prettyJSON(metaJSON.String)
		}
		if t, err := time.Parse(time.RFC3339, eventTime); err == nil {
			// Show audit times in the dashboard's display zone (set via TZ env,
			// defaults to Australia/Sydney in docker-compose). Without this
			// conversion, time.Parse returns UTC even when TZ is set globally.
			ev.Time = t.In(time.Local)
		}
		key := ev.RunID
		if key == "" {
			key = "(no run id)"
		}
		group, ok := groups[key]
		if !ok {
			group = &auditRunGroup{RunID: key, FirstTime: ev.Time, LastTime: ev.Time}
			groups[key] = group
			order = append(order, key)
		}
		group.Events = append(group.Events, ev)
		group.EventCount++
		if ev.Time.Before(group.FirstTime) {
			group.FirstTime = ev.Time
		}
		if ev.Time.After(group.LastTime) {
			group.LastTime = ev.Time
		}
		if strings.EqualFold(ev.Level, "error") {
			group.HasError = true
		}
	}
	if err := rows.Err(); err != nil {
		s.logger.Printf("audit rows: %v", err)
	}

	runs := make([]auditRunGroup, 0, len(order))
	for _, key := range order {
		runs = append(runs, *groups[key])
	}

	s.render(w, "audit", auditData{
		Title:       "Audit",
		Active:      "audit",
		Runs:        runs,
		LevelFilter: levelFilter,
		RunFilter:   runFilter,
		Levels:      []string{"info", "warn", "error"},
	})
}

func prettyJSON(raw string) string {
	var parsed any
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return raw
	}
	out, err := json.MarshalIndent(parsed, "", "  ")
	if err != nil {
		return raw
	}
	return string(out)
}
