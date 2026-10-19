// Package automation implements durable, confirmation-triggered email automations.
package automation

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"io"
	"net"
	"net/textproto"
	"time"

	"github.com/jmoiron/sqlx"
)

// Schema is shared by fresh installation, migrations and integration tests.
//
//go:embed schema.sql
var Schema string

type Rule struct {
	ID           int            `db:"id" json:"id"`
	Name         string         `db:"name" json:"name"`
	ListID       int            `db:"list_id" json:"list_id"`
	TemplateID   int            `db:"template_id" json:"template_id"`
	Enabled      bool           `db:"enabled" json:"enabled"`
	DelaySeconds int            `db:"delay_seconds" json:"delay_seconds"`
	FromEmail    string         `db:"from_email" json:"from_email"`
	Data         map[string]any `json:"data"`
}

type Job struct {
	ID           int64  `db:"id" json:"id"`
	AutomationID int    `db:"automation_id" json:"automation_id"`
	SubscriberID int    `db:"subscriber_id" json:"subscriber_id"`
	ListID       int    `db:"list_id" json:"list_id"`
	Attempts     int    `db:"attempts" json:"attempts"`
	Snapshot     []byte `db:"snapshot" json:"-"`
}

type Snapshot struct {
	Subject   string         `json:"subject"`
	Body      string         `json:"body"`
	FromEmail string         `json:"from_email"`
	Data      map[string]any `json:"data"`
}

// ConfirmSQL changes only genuinely pending subscriptions. Event and enrollment
// writes commit with the confirmation, even if the process exits immediately after.
// Imports / preconfirmed API inserts do not enter this path and never backfill jobs.
const ConfirmSQL = `WITH candidates AS MATERIALIZED (
 SELECT sl.subscriber_id, sl.list_id FROM subscriber_lists sl
 JOIN subscribers s ON s.id=sl.subscriber_id JOIN lists l ON l.id=sl.list_id
 WHERE s.uuid=$1::uuid AND l.uuid=ANY($2::uuid[]) AND sl.status='unconfirmed'
 AND s.status='enabled' AND l.status='active'
 FOR UPDATE OF sl
), confirmed AS (
 UPDATE subscriber_lists sl SET status='confirmed', meta=sl.meta || $3::jsonb, updated_at=NOW()
 FROM candidates c WHERE sl.subscriber_id=c.subscriber_id AND sl.list_id=c.list_id
 RETURNING sl.subscriber_id,sl.list_id
), events AS (
 INSERT INTO automation_events(kind,subscriber_id,list_id)
 SELECT 'subscription_confirmed',subscriber_id,list_id FROM confirmed RETURNING *
)
INSERT INTO automation_jobs(automation_id,event_id,subscriber_id,list_id,scheduled_at,snapshot)
SELECT a.id,e.id,e.subscriber_id,e.list_id,NOW()+make_interval(secs=>a.delay_seconds),
 jsonb_build_object('subject',t.subject,'body',t.body,'from_email',a.from_email,'data',a.data)
FROM events e JOIN automations a ON a.list_id=e.list_id AND a.enabled
JOIN templates t ON t.id=a.template_id AND t.type='tx'
ON CONFLICT(automation_id,subscriber_id) DO NOTHING`

// Worker deliberately does not automatically resend an interrupted in-flight job:
// SMTP acceptance may have happened before the process could record its result.
type Worker struct {
	DB      *sqlx.DB
	Send    func(context.Context, Job) error
	OnError func(error)
}

func (w *Worker) Run(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		if err := w.Tick(ctx); err != nil && w.OnError != nil && ctx.Err() == nil {
			w.OnError(err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (w *Worker) Tick(ctx context.Context) error {
	// Recovery is recorded, visible and requires an explicit administrator decision.
	if _, err := w.DB.ExecContext(ctx, `WITH recovered AS (
 UPDATE automation_jobs SET status='uncertain',last_error='Worker interrupted; SMTP acceptance unknown. Review before retrying.',updated_at=NOW()
 WHERE status='sending' AND updated_at<NOW()-INTERVAL '10 minutes' RETURNING id)
 INSERT INTO automation_attempts(job_id,status,error) SELECT id,'uncertain','Worker interrupted; review required' FROM recovered`); err != nil {
		return err
	}
	for range 10 {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var j Job
		err := w.DB.GetContext(ctx, &j, `WITH due AS (
   SELECT j.id FROM automation_jobs j JOIN automations a ON a.id=j.automation_id
   WHERE a.enabled AND j.status IN ('pending','retry') AND j.scheduled_at<=NOW()
   ORDER BY j.scheduled_at,j.id FOR UPDATE OF j SKIP LOCKED LIMIT 1)
   UPDATE automation_jobs j SET status='sending',attempts=attempts+1,updated_at=NOW()
   FROM due WHERE j.id=due.id RETURNING j.id,j.automation_id,j.subscriber_id,j.list_id,j.attempts,j.snapshot`)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		eligible := false
		if err = w.DB.GetContext(ctx, &eligible, `SELECT EXISTS(
    SELECT 1 FROM automations a JOIN subscriber_lists sl ON sl.list_id=a.list_id
    JOIN subscribers s ON s.id=sl.subscriber_id JOIN lists l ON l.id=sl.list_id
    WHERE a.id=$1 AND sl.subscriber_id=$2 AND a.list_id=$3 AND a.enabled AND sl.status='confirmed'
    AND s.status='enabled' AND l.status='active')`, j.AutomationID, j.SubscriberID, j.ListID); err != nil {
			return err
		}
		status, detail := "cancelled", "Subscriber no longer eligible or automation paused"
		if eligible {
			err = w.Send(ctx, j)
			status, detail = Outcome(err, j.Attempts)
		}
		tx, err := w.DB.BeginTxx(ctx, nil)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE automation_jobs SET status=$2,last_error=$3,updated_at=NOW(),
   scheduled_at=CASE WHEN $2='retry' THEN NOW()+make_interval(secs=>$4) ELSE scheduled_at END,
   sent_at=CASE WHEN $2='sent' THEN NOW() ELSE sent_at END WHERE id=$1`, j.ID, status, detail, RetrySeconds(j.Attempts))
		if err == nil {
			_, err = tx.ExecContext(ctx, `INSERT INTO automation_attempts(job_id,status,error) VALUES($1,$2,$3)`, j.ID, status, detail)
		}
		if err != nil {
			tx.Rollback()
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func RetrySeconds(attempt int) int {
	if attempt > 7 {
		attempt = 7
	}
	if attempt < 1 {
		attempt = 1
	}
	return 30 * (1 << (attempt - 1))
}

// Outcome retries only definite temporary SMTP rejections. A timeout/EOF during
// delivery is ambiguous and is surfaced instead of risking an automatic duplicate.
func Outcome(err error, attempt int) (string, string) {
	if err == nil {
		return "sent", ""
	}
	detail := err.Error()
	if len(detail) > 1000 {
		detail = detail[:1000]
	}
	var smtpErr *textproto.Error
	if errors.As(err, &smtpErr) {
		if smtpErr.Code >= 400 && smtpErr.Code < 500 && attempt < 5 {
			return "retry", detail
		}
		return "failed", detail
	}
	var networkErr net.Error
	if errors.As(err, &networkErr) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "uncertain", detail
	}
	return "failed", detail
}
