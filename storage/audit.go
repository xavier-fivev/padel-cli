package storage

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

type AuditEvent struct {
	ID           string         `json:"id"`
	RunID        string         `json:"run_id"`
	EventTimeUTC string         `json:"event_time_utc"`
	Level        string         `json:"level"`
	Decision     string         `json:"decision"`
	Message      string         `json:"message"`
	TargetDate   string         `json:"target_date"`
	SlotTime     string         `json:"slot_time"`
	VenueID      string         `json:"venue_id"`
	Metadata     map[string]any `json:"metadata,omitempty"`
}

func EnsureAuditSchema(db *sql.DB) error {
	createTable := `
CREATE TABLE IF NOT EXISTS auto_book_audit (
  id TEXT PRIMARY KEY,
  run_id TEXT,
  event_time_utc TEXT,
  level TEXT,
  decision TEXT,
  message TEXT,
  target_date TEXT,
  slot_time TEXT,
  venue_id TEXT,
  metadata_json TEXT
);`
	if _, err := db.Exec(createTable); err != nil {
		return fmt.Errorf("create auto_book_audit table: %w", err)
	}
	if _, err := db.Exec("CREATE INDEX IF NOT EXISTS idx_auto_book_audit_run ON auto_book_audit(run_id);"); err != nil {
		return fmt.Errorf("create auto_book_audit run index: %w", err)
	}
	if _, err := db.Exec("CREATE INDEX IF NOT EXISTS idx_auto_book_audit_target ON auto_book_audit(target_date);"); err != nil {
		return fmt.Errorf("create auto_book_audit target index: %w", err)
	}
	return nil
}

func AddAuditEvent(db *sql.DB, event AuditEvent) error {
	if event.ID == "" {
		event.ID = auditID()
	}
	if event.EventTimeUTC == "" {
		event.EventTimeUTC = time.Now().UTC().Format(time.RFC3339)
	}
	metadata := "{}"
	if len(event.Metadata) > 0 {
		encoded, err := json.Marshal(event.Metadata)
		if err != nil {
			return fmt.Errorf("encode audit metadata: %w", err)
		}
		metadata = string(encoded)
	}

	_, err := db.Exec(`
INSERT INTO auto_book_audit (
  id, run_id, event_time_utc, level, decision, message, target_date, slot_time, venue_id, metadata_json
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?);`,
		event.ID,
		event.RunID,
		event.EventTimeUTC,
		event.Level,
		event.Decision,
		event.Message,
		event.TargetDate,
		event.SlotTime,
		event.VenueID,
		metadata,
	)
	return err
}

func auditID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("audit_%d", time.Now().UnixNano())
	}
	return fmt.Sprintf("audit_%d_%s", time.Now().Unix(), hex.EncodeToString(buf))
}
