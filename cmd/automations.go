package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/mail"
	"net/textproto"
	"net/url"
	"strings"

	"github.com/knadh/koanf/v2"
	"github.com/knadh/listmonk/internal/auth"
	"github.com/knadh/listmonk/internal/automation"
	"github.com/knadh/listmonk/internal/messenger/email"
	"github.com/knadh/listmonk/internal/notifs"
	"github.com/knadh/listmonk/models"
	"github.com/labstack/echo/v4"
)

// Automation administration is deliberately super-admin-only in this release.
// It combines list access, sending and system-template management privileges.
func automationAdmin(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		if auth.GetUser(c).UserRole.ID != auth.SuperAdminRoleID {
			return echo.NewHTTPError(http.StatusForbidden, "Automation administration requires a super administrator")
		}
		return next(c)
	}
}

func (a *App) GetAutomations(c echo.Context) error {
	// Query JSON to preserve snake_case API fields and nested template data.
	var out json.RawMessage
	err := a.db.Get(&out, `SELECT COALESCE(json_agg(x ORDER BY x.id),'[]') FROM (
 SELECT a.*,l.name AS list_name,t.name AS template_name FROM automations a JOIN lists l ON l.id=a.list_id JOIN templates t ON t.id=a.template_id) x`)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, okResp{out})
}

func (a *App) SaveAutomation(c echo.Context) error {
	var r automation.Rule
	if err := c.Bind(&r); err != nil {
		return err
	}
	if len(strings.TrimSpace(r.Name)) == 0 || len(r.Name) > 200 || r.DelaySeconds < 0 || r.DelaySeconds > 2592000 {
		return echo.NewHTTPError(400, "Name and delay (0–2592000 seconds) are required")
	}
	if len(r.FromEmail) > 0 {
		if _, err := mail.ParseAddress(r.FromEmail); err != nil || strings.ContainsAny(r.FromEmail, "\r\n") {
			return echo.NewHTTPError(400, "Invalid sender address")
		}
	}
	if r.Data == nil {
		r.Data = map[string]any{}
	}
	data, err := json.Marshal(r.Data)
	if err != nil || len(data) > 65536 {
		return echo.NewHTTPError(400, "Invalid automation data")
	}
	var t models.Template
	if err = a.db.Get(&t, "SELECT id,name,type,subject,body FROM templates WHERE id=$1 AND type='tx'", r.TemplateID); err != nil {
		return echo.NewHTTPError(400, "Select a transactional email template")
	}
	if !strings.Contains(t.Body, ".Tx.Data.unsubscribe_url") {
		return echo.NewHTTPError(400, "The welcome template must contain {{ .Tx.Data.unsubscribe_url }}")
	}
	if err = t.Compile(a.manager.GenericTemplateFuncs()); err != nil {
		return echo.NewHTTPError(400, err.Error())
	}
	previewData := map[string]any{}
	for k, v := range r.Data {
		previewData[k] = v
	}
	const previewUnsub = "https://example.invalid/preferences"
	previewData["unsubscribe_url"] = previewUnsub
	preview := models.TxMessage{Data: previewData}
	if err := preview.Render(models.Subscriber{Name: "Sample subscriber", Email: "subscriber@example.invalid", Attribs: models.JSON{}}, &t, a.manager.GenericTemplateFuncs()); err != nil {
		return echo.NewHTTPError(400, "Cannot render welcome template: "+err.Error())
	}
	if !automation.HasLink(string(preview.Body), previewUnsub) {
		return echo.NewHTTPError(400, "Welcome template must link to {{ .Tx.Data.unsubscribe_url }}")
	}
	var exists bool
	if err = a.db.Get(&exists, "SELECT EXISTS(SELECT 1 FROM lists WHERE id=$1 AND optin='double' AND status='active')", r.ListID); err != nil {
		return err
	}
	if !exists {
		return echo.NewHTTPError(400, "Select an active double opt-in list")
	}
	if c.Param("id") != "" {
		r.ID = getID(c)
		res, err := a.db.Exec(`UPDATE automations SET name=$2,list_id=$3,template_id=$4,enabled=$5,delay_seconds=$6,from_email=$7,data=$8,updated_at=NOW() WHERE id=$1`, getID(c), strings.TrimSpace(r.Name), r.ListID, r.TemplateID, r.Enabled, r.DelaySeconds, r.FromEmail, data)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return echo.NewHTTPError(404, "Automation not found")
		}
	} else {
		if err = a.db.Get(&r.ID, `INSERT INTO automations(name,list_id,template_id,enabled,delay_seconds,from_email,data) VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING id`, strings.TrimSpace(r.Name), r.ListID, r.TemplateID, r.Enabled, r.DelaySeconds, r.FromEmail, data); err != nil {
			return err
		}
	}
	return c.JSON(200, okResp{r})
}

func (a *App) GetAutomationJobs(c echo.Context) error {
	var out json.RawMessage
	err := a.db.Get(&out, `SELECT COALESCE(json_agg(x ORDER BY x.id DESC),'[]') FROM (
 SELECT j.id,j.automation_id,a.name AS automation_name,j.subscriber_id,s.email,j.list_id,j.status,j.attempts,j.scheduled_at,j.last_error,j.created_at,j.updated_at,j.sent_at,
 COALESCE((SELECT json_agg(p ORDER BY p.id) FROM automation_attempts p WHERE p.job_id=j.id),'[]') AS history
 FROM automation_jobs j JOIN automations a ON a.id=j.automation_id JOIN subscribers s ON s.id=j.subscriber_id
 ORDER BY j.id DESC LIMIT 200) x`)
	if err != nil {
		return err
	}
	return c.JSON(200, okResp{out})
}

func (a *App) AutomationJobAction(c echo.Context) error {
	var req struct {
		Action      string `json:"action"`
		Acknowledge bool   `json:"acknowledge_duplicate"`
	}
	if err := c.Bind(&req); err != nil {
		return err
	}
	var query string
	switch req.Action {
	case "cancel":
		query = `UPDATE automation_jobs SET status='cancelled',last_error='Cancelled by administrator',updated_at=NOW() WHERE id=$1 AND status IN ('pending','retry','failed','uncertain') RETURNING id`
	case "retry":
		if !req.Acknowledge {
			return echo.NewHTTPError(400, "Manual retries can duplicate a previously accepted email; acknowledge_duplicate is required")
		}
		query = `UPDATE automation_jobs SET status='pending',scheduled_at=NOW(),last_error='',updated_at=NOW() WHERE id=$1 AND status IN ('failed','uncertain','cancelled') RETURNING id`
	default:
		return echo.NewHTTPError(400, "Action must be cancel or retry")
	}
	var id int64
	tx, err := a.db.Beginx()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = tx.Get(&id, query, getID(c)); err == sql.ErrNoRows {
		return echo.NewHTTPError(409, "Job is missing or cannot be changed in its current state")
	} else if err != nil {
		return err
	}
	if _, err = tx.Exec(`INSERT INTO automation_attempts(job_id,status,error) VALUES($1,$2,'Administrator action')`, id, req.Action); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return c.JSON(200, okResp{true})
}

func (a *App) sendAutomation(ctx context.Context, j automation.Job) error {
	if a.automationEmail == nil {
		return fmt.Errorf("no enabled SMTP servers for automation delivery")
	}
	var snap automation.Snapshot
	if err := json.Unmarshal(j.Snapshot, &snap); err != nil {
		return err
	}
	sub, err := a.core.GetSubscriber(j.SubscriberID, "", "")
	if err != nil {
		return err
	}
	unsub := fmt.Sprintf(a.urlCfg.UnsubURL, dummyUUID, sub.UUID)
	f := a.manager.GenericTemplateFuncs()
	t := models.Template{Body: snap.Body, Subject: snap.Subject}
	if err = t.Compile(f); err != nil {
		return err
	}
	if snap.Data == nil {
		snap.Data = map[string]any{}
	}
	snap.Data["unsubscribe_url"] = unsub
	msg := models.TxMessage{Data: snap.Data, ContentType: models.CampaignContentTypeHTML}
	if err = msg.Render(sub, &t, f); err != nil {
		return err
	}
	if strings.ContainsAny(msg.Subject, "\r\n") {
		return fmt.Errorf("invalid rendered email subject")
	}
	if !automation.HasLink(string(msg.Body), unsub) {
		return fmt.Errorf("welcome template must link to {{ .Tx.Data.unsubscribe_url }}")
	}
	from := snap.FromEmail
	if from == "" {
		from = a.cfg.FromEmail
	}
	hdr := textproto.MIMEHeader{}
	hdr.Set("List-Unsubscribe", "<"+unsub+">")
	hdr.Set("List-Unsubscribe-Post", "List-Unsubscribe=One-Click")
	host := "localhost"
	if u, err := url.Parse(ko.String("app.root_url")); err == nil && u.Hostname() != "" {
		host = u.Hostname()
	}
	hdr.Set("Message-ID", fmt.Sprintf("<automation-%d@%s>", j.ID, host))
	hdr.Set(models.EmailHeaderSubscriberUUID, sub.UUID)
	// Direct synchronous messenger result is recorded before the next job.
	return a.automationEmail.Push(models.Message{From: from, To: []string{sub.Email}, Subject: msg.Subject, Body: msg.Body, ContentType: models.CampaignContentTypeHTML, Headers: hdr, Subscriber: sub})
}

type confirmationConfig struct {
	ListID  int    `db:"list_id" json:"list_id"`
	Subject string `db:"subject" json:"subject"`
	Body    string `db:"body" json:"body"`
	Enabled bool   `db:"enabled" json:"enabled"`
}

func confirmationPreviewData() automation.ConfirmationData {
	return automation.ConfirmationData{Subscriber: models.Subscriber{Name: "Sample subscriber", Email: "subscriber@example.invalid"}, Lists: []models.List{{Name: "Sample list"}}, OptinURL: "https://example.invalid/confirm", UnsubURL: "https://example.invalid/preferences"}
}
func (a *App) GetConfirmationTemplates(c echo.Context) error {
	out := []confirmationConfig{}
	if err := a.db.Select(&out, "SELECT list_id,subject,body,enabled FROM list_confirmation_templates ORDER BY list_id"); err != nil {
		return err
	}
	return c.JSON(200, okResp{out})
}
func (a *App) SaveConfirmationTemplate(c echo.Context) error {
	var v confirmationConfig
	if err := c.Bind(&v); err != nil {
		return err
	}
	v.ListID = getID(c)
	if len(v.Body) > 1048576 || len(v.Subject) > 500 {
		return echo.NewHTTPError(400, "Template is too large")
	}
	if _, _, err := automation.RenderConfirmation(v.Subject, v.Body, confirmationPreviewData()); err != nil {
		return echo.NewHTTPError(400, err.Error())
	}
	var exists bool
	if err := a.db.Get(&exists, "SELECT EXISTS(SELECT 1 FROM lists WHERE id=$1 AND optin='double')", v.ListID); err != nil {
		return err
	}
	if !exists {
		return echo.NewHTTPError(400, "Select a double opt-in list")
	}
	_, err := a.db.Exec(`INSERT INTO list_confirmation_templates(list_id,subject,body,enabled) VALUES($1,$2,$3,$4) ON CONFLICT(list_id) DO UPDATE SET subject=EXCLUDED.subject,body=EXCLUDED.body,enabled=EXCLUDED.enabled,updated_at=NOW()`, v.ListID, v.Subject, v.Body, v.Enabled)
	if err != nil {
		return err
	}
	return c.JSON(200, okResp{v})
}
func (a *App) PreviewConfirmationTemplate(c echo.Context) error {
	var v confirmationConfig
	if err := c.Bind(&v); err != nil {
		return err
	}
	subject, body, err := automation.RenderConfirmation(v.Subject, v.Body, confirmationPreviewData())
	if err != nil {
		return echo.NewHTTPError(400, err.Error())
	}
	return c.JSON(200, okResp{map[string]string{"subject": subject, "body": body}})
}
func (a *App) TestConfirmationTemplate(c echo.Context) error {
	var v struct {
		Subject string `json:"subject"`
		Body    string `json:"body"`
		To      string `json:"to"`
	}
	if err := c.Bind(&v); err != nil {
		return err
	}
	if _, err := mail.ParseAddress(v.To); err != nil || strings.ContainsAny(v.To, "\r\n") {
		return echo.NewHTTPError(400, "Invalid test recipient")
	}
	subject, body, err := automation.RenderConfirmation(v.Subject, v.Body, confirmationPreviewData())
	if err != nil {
		return echo.NewHTTPError(400, err.Error())
	}
	// Preview URLs never confirm/unsubscribe a real contact.
	if err = notifs.SendHTML(v.To, "[TEST] "+subject, body, textproto.MIMEHeader{}); err != nil {
		return err
	}
	return c.JSON(200, okResp{true})
}

func sendCustomOptins(sub models.Subscriber, lists []models.List, u *UrlConfig, unsubHeader bool) ([]models.List, int, error) {
	remaining := []models.List{}
	sent := 0
	for _, l := range lists {
		var v confirmationConfig
		err := db.Get(&v, "SELECT list_id,subject,body,enabled FROM list_confirmation_templates WHERE list_id=$1 AND enabled", l.ID)
		if err == sql.ErrNoRows {
			remaining = append(remaining, l)
			continue
		}
		if err != nil {
			return nil, sent, err
		}
		params := url.Values{"l": []string{l.UUID}}
		data := automation.ConfirmationData{Subscriber: sub, Lists: []models.List{l}, OptinURL: fmt.Sprintf(u.OptinURL, sub.UUID, params.Encode()), UnsubURL: fmt.Sprintf(u.UnsubURL, dummyUUID, sub.UUID)}
		subject, body, err := automation.RenderConfirmation(v.Subject, v.Body, data)
		if err != nil {
			return nil, sent, err
		}
		hdr := textproto.MIMEHeader{}
		hdr.Set(models.EmailHeaderSubscriberUUID, sub.UUID)
		if unsubHeader {
			hdr.Set("List-Unsubscribe", "<"+data.UnsubURL+">")
			hdr.Set("List-Unsubscribe-Post", "List-Unsubscribe=One-Click")
		}
		if err = notifs.SendHTML(sub.Email, subject, body, hdr); err != nil {
			return nil, sent, err
		}
		sent++
	}
	return remaining, sent, nil
}

// A dedicated pool makes the durable queue the only retry owner. The campaign
// SMTP pool can retry broken connections internally, which is unsafe for a
// delivery whose acceptance is unknown.
func initAutomationEmail() *email.Emailer {
	servers := []email.Server{}
	for _, item := range ko.Slices("smtp") {
		if !item.Bool("enabled") {
			continue
		}
		var s email.Server
		if err := item.UnmarshalWithConf("", &s, koanf.UnmarshalConf{Tag: "json"}); err != nil {
			lo.Fatalf("automation SMTP config: %v", err)
		}
		servers = append(servers, s)
	}
	if len(servers) == 0 {
		return nil
	}
	m, err := email.NewSingleAttempt("automation-email", servers...)
	if err != nil {
		lo.Fatalf("automation SMTP initialization: %v", err)
	}
	return m
}
