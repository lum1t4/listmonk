package automation

import (
	"context"
	"fmt"
	"io"
	"net/textproto"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/knadh/listmonk/models"
	"github.com/lib/pq"
)

func TestOutcome(t *testing.T) {
	for _, c := range []struct {
		err     error
		attempt int
		want    string
	}{
		{nil, 1, "sent"}, {&textproto.Error{Code: 451, Msg: "temporary"}, 1, "retry"},
		{fmt.Errorf("wrapped: %w", &textproto.Error{Code: 451, Msg: "temporary"}), 4, "retry"},
		{&textproto.Error{Code: 451, Msg: "temporary"}, 5, "failed"},
		{&textproto.Error{Code: 550, Msg: "rejected"}, 1, "failed"}, {io.EOF, 1, "uncertain"},
		{context.DeadlineExceeded, 1, "uncertain"}, {fmt.Errorf("render failed"), 1, "failed"},
	} {
		got, _ := Outcome(c.err, c.attempt)
		if got != c.want {
			t.Errorf("%v: got %s want %s", c.err, got, c.want)
		}
	}
	if RetrySeconds(0) != 30 || RetrySeconds(3) != 120 || RetrySeconds(100) != 1920 {
		t.Fatal("retry backoff out of range")
	}
	_, detail := Outcome(fmt.Errorf("%s", strings.Repeat("x", 2000)), 1)
	if len(detail) != 1000 {
		t.Fatal("unbounded error")
	}
}

func TestConfirmation(t *testing.T) {
	d := ConfirmationData{Subscriber: models.Subscriber{Name: "<script>alert(1)</script>"}, OptinURL: "https://example.test/confirm?a=1&b=2", UnsubURL: "https://example.test/preferences"}
	subject, body, err := RenderConfirmation("Welcome {{ .Subscriber.Name }}", `<p>{{ .Subscriber.Name }}</p><a href="{{ .OptinURL }}">Confirm</a>`, d)
	if err != nil || !strings.Contains(subject, "<script>") || strings.Contains(body, "<script>") || !strings.Contains(body, "&amp;b=2") {
		t.Fatalf("escaping: %s %s %v", subject, body, err)
	}
	for _, body := range []string{`<!-- {{ .OptinURL }} -->`, `{{ .OptinURL }}`, `<a href="{{ .UnsubURL }}">Wrong</a>`, `<a href="{{ .Missing }}">Broken</a>`, `{{`} {
		if _, _, err := RenderConfirmation("Confirm", body, d); err == nil {
			t.Errorf("accepted invalid body %q", body)
		}
	}
	for _, subject := range []string{"", "Bad\r\nBcc: somebody@example.test", "{{ .Missing }}"} {
		if _, _, err := RenderConfirmation(subject, `<a href="{{ .OptinURL }}">Confirm</a>`, d); err == nil {
			t.Errorf("accepted subject %q", subject)
		}
	}
	d.Subscriber.Name = "bad\nsubject"
	if _, _, err := RenderConfirmation("{{ .Subscriber.Name }}", `<a href="{{ .OptinURL }}">Confirm</a>`, d); err == nil {
		t.Fatal("accepted rendered header injection")
	}
}

var schemaCounter atomic.Int64

func testDB(t *testing.T) *sqlx.DB {
	t.Helper()
	dsn := os.Getenv("AUTOMATION_TEST_DSN")
	if dsn == "" {
		t.Skip("AUTOMATION_TEST_DSN not set")
	}
	db, err := sqlx.Connect("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("automation_test_%d_%d", time.Now().UnixNano(), schemaCounter.Add(1))
	db.MustExec("CREATE SCHEMA " + schema)
	db.Close()
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	db, err = sqlx.Connect("postgres", dsn+sep+"search_path="+schema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.MustExec("DROP SCHEMA " + schema + " CASCADE"); db.Close() })
	db.MustExec(`CREATE TABLE subscribers(id SERIAL PRIMARY KEY,uuid UUID NOT NULL,status TEXT NOT NULL DEFAULT 'enabled');
 CREATE TABLE lists(id SERIAL PRIMARY KEY,uuid UUID NOT NULL,status TEXT NOT NULL DEFAULT 'active');
 CREATE TABLE templates(id SERIAL PRIMARY KEY,type TEXT NOT NULL,subject TEXT NOT NULL,body TEXT NOT NULL);
 CREATE TABLE subscriber_lists(subscriber_id INTEGER REFERENCES subscribers(id) ON DELETE CASCADE,list_id INTEGER REFERENCES lists(id) ON DELETE CASCADE,status TEXT NOT NULL DEFAULT 'unconfirmed',meta JSONB NOT NULL DEFAULT '{}',updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),PRIMARY KEY(subscriber_id,list_id));`)
	db.MustExec(Schema)
	db.MustExec(Schema) // Migration must be idempotent.
	db.MustExec(`INSERT INTO subscribers(uuid) VALUES('00000000-0000-4000-a000-000000000001');INSERT INTO lists(uuid) VALUES('00000000-0000-4000-a000-000000000002');INSERT INTO subscriber_lists VALUES(1,1,'unconfirmed','{}',NOW());INSERT INTO templates(type,subject,body) VALUES('tx','Original subject','Original body');INSERT INTO automations(name,list_id,template_id,enabled) VALUES('Welcome',1,1,true)`)
	return db
}
func confirm(db *sqlx.DB) error {
	_, err := db.Exec(ConfirmSQL, "00000000-0000-4000-a000-000000000001", pq.Array([]string{"00000000-0000-4000-a000-000000000002"}), `{"ip":"test"}`)
	return err
}
func scalar(t *testing.T, db *sqlx.DB, q string) string {
	t.Helper()
	var s string
	if err := db.Get(&s, q); err != nil {
		t.Fatal(err)
	}
	return s
}
func TestConcurrentConfirmationAndDelivery(t *testing.T) {
	db := testDB(t)
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := confirm(db); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if scalar(t, db, "SELECT count(*) FROM automation_events") != "1" || scalar(t, db, "SELECT count(*) FROM automation_jobs") != "1" {
		t.Fatal("confirmation was enrolled more than once")
	}
	db.MustExec("UPDATE templates SET subject='Changed',body='Changed'")
	var sent atomic.Int64
	w := Worker{DB: db, Send: func(_ context.Context, j Job) error {
		sent.Add(1)
		if !strings.Contains(string(j.Snapshot), "Original subject") {
			t.Error("snapshot changed")
		}
		return nil
	}}
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := w.Tick(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if sent.Load() != 1 || scalar(t, db, "SELECT status FROM automation_jobs") != "sent" || scalar(t, db, "SELECT count(*) FROM automation_attempts") != "1" {
		t.Fatalf("delivery not exactly one: %d", sent.Load())
	}
	db.MustExec("UPDATE subscriber_lists SET status='unconfirmed'")
	if err := confirm(db); err != nil {
		t.Fatal(err)
	}
	if scalar(t, db, "SELECT count(*) FROM automation_jobs") != "1" {
		t.Fatal("reconfirmation repeated welcome")
	}
	db.MustExec("DELETE FROM subscribers")
	if scalar(t, db, "SELECT count(*) FROM automation_jobs") != "0" || scalar(t, db, "SELECT count(*) FROM automation_attempts") != "0" {
		t.Fatal("subscriber removal left delivery data")
	}
}
func TestEligibility(t *testing.T) {
	for _, c := range []struct{ name, change, want string }{
		{"unsubscribe", "UPDATE subscriber_lists SET status='unsubscribed'", "cancelled"},
		{"blocklist", "UPDATE subscribers SET status='blocklisted'", "cancelled"},
		{"archive", "UPDATE lists SET status='archived'", "cancelled"},
		{"pause", "UPDATE automations SET enabled=false", "pending"},
		{"delay", "UPDATE automation_jobs SET scheduled_at=NOW()+INTERVAL '1 hour'", "pending"},
	} {
		t.Run(c.name, func(t *testing.T) {
			db := testDB(t)
			if err := confirm(db); err != nil {
				t.Fatal(err)
			}
			db.MustExec(c.change)
			calls := 0
			w := Worker{DB: db, Send: func(context.Context, Job) error { calls++; return nil }}
			if err := w.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			if scalar(t, db, "SELECT status FROM automation_jobs") != c.want || calls != 0 {
				t.Fatal("ineligible subscriber was sent an email")
			}
			if c.name == "pause" {
				db.MustExec("UPDATE automations SET enabled=true")
				if err := w.Tick(context.Background()); err != nil {
					t.Fatal(err)
				}
				if calls != 1 {
					t.Fatal("resume failed")
				}
			}
		})
	}
}
func TestRecoveryAndRetries(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		want string
	}{{"temporary", &textproto.Error{Code: 451, Msg: "later"}, "retry"}, {"permanent", &textproto.Error{Code: 550, Msg: "no"}, "failed"}, {"ambiguous", io.EOF, "uncertain"}} {
		t.Run(c.name, func(t *testing.T) {
			db := testDB(t)
			if err := confirm(db); err != nil {
				t.Fatal(err)
			}
			calls := 0
			w := Worker{DB: db, Send: func(context.Context, Job) error { calls++; return c.err }}
			if err := w.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			if scalar(t, db, "SELECT status FROM automation_jobs") != c.want {
				t.Fatal("wrong outcome")
			}
			if err := w.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatal("immediate or unsafe resend")
			}
			if c.want == "retry" {
				db.MustExec("UPDATE automation_jobs SET scheduled_at=NOW(),attempts=4")
				if err := w.Tick(context.Background()); err != nil {
					t.Fatal(err)
				}
				if scalar(t, db, "SELECT status FROM automation_jobs") != "failed" {
					t.Fatal("retry limit exceeded")
				}
			}
		})
	}
	db := testDB(t)
	if err := confirm(db); err != nil {
		t.Fatal(err)
	}
	db.MustExec("UPDATE automation_jobs SET status='sending',updated_at=NOW()-INTERVAL '11 minutes'")
	w := Worker{DB: db, Send: func(context.Context, Job) error { t.Fatal("interrupted delivery resent"); return nil }}
	if err := w.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if scalar(t, db, "SELECT status FROM automation_jobs") != "uncertain" {
		t.Fatal("interrupted job not recovered")
	}
}
func TestNoEnrollmentForExistingOrIneligibleSubscribers(t *testing.T) {
	for _, change := range []string{"UPDATE subscriber_lists SET status='confirmed'", "UPDATE subscriber_lists SET status='unsubscribed'", "UPDATE subscribers SET status='blocklisted'", "UPDATE lists SET status='archived'"} {
		t.Run(change, func(t *testing.T) {
			db := testDB(t)
			db.MustExec(change)
			if err := confirm(db); err != nil {
				t.Fatal(err)
			}
			if scalar(t, db, "SELECT count(*) FROM automation_jobs") != "0" {
				t.Fatal("unexpected enrollment")
			}
		})
	}
}
func TestConfirmationRollsBackWhenEnrollmentFails(t *testing.T) {
	db := testDB(t)
	db.MustExec(`ALTER TABLE automation_jobs ADD CONSTRAINT reject_jobs CHECK(false)`)
	if err := confirm(db); err == nil {
		t.Fatal("expected failure")
	}
	if scalar(t, db, "SELECT status FROM subscriber_lists") != "unconfirmed" || scalar(t, db, "SELECT count(*) FROM automation_events") != "0" {
		t.Fatal("confirmation and enrollment are not atomic")
	}
}
