package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

// Event dispositions are sanitized machine-readable evidence, never provider bodies.
func (c *authLifecycleController) recordEventOutcome(ctx context.Context, db *sql.DB, event lifecycleEvent, processed bool, outcome, detail string) error {
	retryAt := int64(0)
	if !processed {
		retryAt = c.clock.Now().Add(30 * time.Second).Unix()
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE auth_lifecycle_events SET processed=?,retry_at=?,outcome=?,detail=? WHERE id=? AND (processed<>? OR retry_at<>? OR outcome<>? OR detail<>?)`, boolInt(processed), retryAt, outcome, detail, event.ID, boolInt(processed), retryAt, outcome, detail)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed > 0 {
		// Also invalidate the summary revision so deferred events remain visible.
		if _, err = tx.ExecContext(ctx, `INSERT INTO auth_lifecycle_operations(auth_index,at,action,outcome,detail) VALUES(?,?,?,?,?)`, event.Index, c.clock.Now().Unix(), "event_disposition", outcome, detail); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func pendingLifecycleEvents(ctx context.Context, db *sql.DB, s authLifecycleState) (bool, error) {
	var pending bool
	err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM auth_lifecycle_events WHERE processed=0 AND (auth_index=? OR (auth_index='' AND auth_id=?)))`, s.AuthIndex, s.AuthID).Scan(&pending)
	return pending, err
}

type lifecycleEventDiagnostic struct {
	ID          int64  `json:"id"`
	AuthIndex   string `json:"auth_index"`
	AuthID      string `json:"auth_id"`
	RequestedAt int64  `json:"requested_at"`
	Status      int    `json:"status"`
	Outcome     string `json:"outcome"`
	Detail      string `json:"detail"`
	RetryAt     int64  `json:"retry_at"`
}

func queryLifecycleEventDiagnostics(ctx context.Context, db *sql.DB) (map[string]any, error) {
	counts := map[string]int{}
	rows, err := db.QueryContext(ctx, `SELECT outcome,COUNT(*) FROM auth_lifecycle_events WHERE processed=0 OR outcome IN ('pending_disable','identity_conflict') GROUP BY outcome`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var outcome string
		var count int
		if err = rows.Scan(&outcome, &count); err != nil {
			rows.Close()
			return nil, err
		}
		counts[outcome] = count
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	// Pending evidence gets its own bounded list, so healthy traffic cannot hide it.
	lists := map[string]any{"counts": counts}
	for _, list := range []struct{ name, predicate string }{
		{"pending", "WHERE processed=0 OR outcome IN ('pending_disable','identity_conflict')"},
		{"recent", ""},
	} {
		rows, err = db.QueryContext(ctx, `SELECT id,auth_index,auth_id,requested_at,payload,outcome,detail,retry_at FROM auth_lifecycle_events `+list.predicate+` ORDER BY id DESC LIMIT 100`)
		if err != nil {
			return nil, err
		}
		events := []lifecycleEventDiagnostic{}
		for rows.Next() {
			var e lifecycleEventDiagnostic
			var raw string
			if err = rows.Scan(&e.ID, &e.AuthIndex, &e.AuthID, &e.RequestedAt, &raw, &e.Outcome, &e.Detail, &e.RetryAt); err != nil {
				rows.Close()
				return nil, err
			}
			var d authDecision
			if err = json.Unmarshal([]byte(raw), &d); err != nil {
				rows.Close()
				return nil, err
			}
			e.Status = d.Status
			events = append(events, e)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
		lists[list.name] = events
	}
	return lists, nil
}
