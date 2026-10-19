CREATE TABLE IF NOT EXISTS automations (
 id SERIAL PRIMARY KEY,
 name TEXT NOT NULL CHECK(length(name) BETWEEN 1 AND 200),
 list_id INTEGER NOT NULL REFERENCES lists(id) ON DELETE CASCADE,
 template_id INTEGER NOT NULL REFERENCES templates(id) ON DELETE RESTRICT,
 enabled BOOLEAN NOT NULL DEFAULT FALSE,
 delay_seconds INTEGER NOT NULL DEFAULT 0 CHECK(delay_seconds BETWEEN 0 AND 2592000),
 from_email TEXT NOT NULL DEFAULT '',
 data JSONB NOT NULL DEFAULT '{}',
 created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
 updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE TABLE IF NOT EXISTS automation_events (
 id BIGSERIAL PRIMARY KEY,
 kind TEXT NOT NULL CHECK(kind='subscription_confirmed'),
 subscriber_id INTEGER NOT NULL REFERENCES subscribers(id) ON DELETE CASCADE,
 list_id INTEGER NOT NULL REFERENCES lists(id) ON DELETE CASCADE,
 created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE TABLE IF NOT EXISTS automation_jobs (
 id BIGSERIAL PRIMARY KEY,
 automation_id INTEGER NOT NULL REFERENCES automations(id) ON DELETE CASCADE,
 event_id BIGINT NOT NULL REFERENCES automation_events(id) ON DELETE CASCADE,
 subscriber_id INTEGER NOT NULL REFERENCES subscribers(id) ON DELETE CASCADE,
 list_id INTEGER NOT NULL REFERENCES lists(id) ON DELETE CASCADE,
 status TEXT NOT NULL DEFAULT 'pending' CHECK(status IN ('pending','sending','sent','retry','failed','uncertain','cancelled')),
 scheduled_at TIMESTAMPTZ NOT NULL,
 attempts INTEGER NOT NULL DEFAULT 0,
 snapshot JSONB NOT NULL,
 last_error TEXT NOT NULL DEFAULT '',
 created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
 updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
 sent_at TIMESTAMPTZ,
 UNIQUE(automation_id,subscriber_id)
);
CREATE INDEX IF NOT EXISTS automation_jobs_due ON automation_jobs(scheduled_at) WHERE status IN ('pending','retry');
CREATE INDEX IF NOT EXISTS automation_events_subscriber ON automation_events(subscriber_id,created_at);
CREATE TABLE IF NOT EXISTS automation_attempts (
 id BIGSERIAL PRIMARY KEY,
 job_id BIGINT NOT NULL REFERENCES automation_jobs(id) ON DELETE CASCADE,
 status TEXT NOT NULL,
 error TEXT NOT NULL DEFAULT '',
 created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE TABLE IF NOT EXISTS list_confirmation_templates (
 list_id INTEGER PRIMARY KEY REFERENCES lists(id) ON DELETE CASCADE,
 subject TEXT NOT NULL,
 body TEXT NOT NULL,
 enabled BOOLEAN NOT NULL DEFAULT FALSE,
 updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
