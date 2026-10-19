# Confirmation-triggered automations

This fork adds a generic, native workflow to listmonk:

1. A subscriber submits an existing public subscription form (or an integration requests double opt-in).
2. Listmonk sends its usual confirmation email, optionally using a per-list HTML template.
3. The subscriber confirms using listmonk's native confirmation link.
4. Confirmation and welcome-email enrollment commit in one database transaction.
5. A background worker sends the selected transactional template immediately or after a delay.

There is no store-specific content, coupon generator, WordPress dependency, or external webhook requirement. Put your own welcome message, coupon, branding and template variables in the instance database through the admin UI. Coupon validity and redemption limits remain the responsibility of your commerce system.

## Administration

A super administrator can open **Automations** in the navigation. Other roles cannot access the new API or UI.

- Create an automation, select an active double opt-in list and a transactional template, optionally set a delay (up to 30 days), sender and JSON template data, then enable it.
- Include an actual unsubscribe link using `{{ .Tx.Data.unsubscribe_url }}` in the welcome template. Subscriber fields and existing transactional-template functions are available. Templates are snapshotted when the subscriber confirms: later edits affect future enrollments.
- In **Confirmation emails**, choose a list, edit the subject and HTML, preview it, send a test, and enable the override. Use `{{ .Subscriber.Name }}`, `{{ .OptinURL }}` and `{{ .UnsubURL }}`. An anchor linking to `.OptinURL` is mandatory. Test emails use sample links and cannot confirm a real subscriber. Lists without an enabled override retain the system confirmation email.
- In **Delivery history**, inspect the most recent 200 jobs and their attempts, cancel pending work, or explicitly acknowledge duplicate risk before retrying a failed, cancelled or uncertain job.

Each automation welcomes a subscriber at most once, including after unsubscribe/resubscribe. Existing contacts, imports, API inserts that are already confirmed, and confirmations while an automation is paused are not backfilled. A paused automation holds existing pending jobs until resumed. Disabling enrollment is not cancellation; cancel individual jobs if they must never run.

## Delivery guarantees and limits

The queue is durable in PostgreSQL. Concurrent workers use `FOR UPDATE SKIP LOCKED`. Before sending, the worker checks that the subscriber remains enabled and confirmed in the automation's active list. Deleted subscribers and lists cascade-delete their associated automation data. Rule changes that move an automation to another list cancel old jobs when processed.

A successful send means SMTP accepted the message, not that an inbox received it. Temporary SMTP rejections retry with exponential backoff, up to five attempts. Permanent rejections fail. Timeouts, broken connections and interrupted in-flight sends are **uncertain** and are never automatically resent: SMTP cannot guarantee exactly-once delivery. An interrupted `sending` job becomes uncertain after ten minutes. Manual retries use the same Message-ID but recipient providers are not required to deduplicate it. History records manual actions and delivery outcomes.

This release supports one event and one email action, not a general visual journey builder. There is no automatic enrollment of historical contacts, arbitrary event scripting, purchase tracking, coupon issuance, or multi-step branching. UI labels added by this fork are English. Welcome messages use the configured default email messenger; existing provider settings and sender identity still apply. Keep SMTP connection limits appropriate to the provider.

## APIs

All new endpoints require super-admin authentication and use the normal `{"data": ...}` response envelope:

- `GET /api/automations`
- `POST /api/automations`, `PUT /api/automations/:id`: `name`, `list_id`, `template_id`, `enabled`, `delay_seconds`, `from_email`, `data`.
- `GET /api/automation-jobs`
- `POST /api/automation-jobs/:id/actions`: `{"action":"cancel"}` or `{"action":"retry","acknowledge_duplicate":true}`.
- `GET /api/confirmation-templates`
- `PUT /api/confirmation-templates/:id`: `subject`, `body`, `enabled` (ID is the list ID).
- `POST /api/confirmation-templates/preview`: `subject`, `body`.
- `POST /api/confirmation-templates/test`: `subject`, `body`, `to`.

## Build, test and deployment

The base is upstream v6.2.0. The fork identifies as `v6.3.0-automation.1`. Fresh installations and upgrades install the same automation schema. Existing listmonk v5 installations must run the regular upstream upgrade chain as well.

```sh
# A disposable PostgreSQL database, never the production DSN.
AUTOMATION_TEST_DSN='postgres://listmonk:test-only@localhost:5432/automation_test?sslmode=disable' \
  go test -race ./internal/... ./models/...
go vet ./internal/... ./models/... ./cmd/...
docker build -f Dockerfile.fork -t listmonk-fork:local .
bash tests/run-local.sh # disposable stock-v5 upgrade + real HTTP/SMTP + fresh install
```

Database tests create isolated schemas and cover concurrent confirmation and claiming, snapshots, enrollment atomicity, pause/resume, delivery eligibility, retry limits, ambiguous sends, restart recovery and deletion. Set `AUTOMATION_TEST_DSN` to run them; without it, integration tests skip.

The GitHub `Fork container` workflow runs tests against PostgreSQL and publishes `ghcr.io/<owner>/listmonk-automations:sha-<commit>` and `:automation`. Deploy the immutable digest, run `--upgrade --yes`, and preserve the existing configuration and upload volumes. No branded deployment content belongs in this repository.

Before upgrading, back up the database, uploaded files and deployment configuration. A rollback to v5 requires restoring the pre-upgrade database as well as the old image; do not run v5 against the upgraded schema. Keep backups and credentials out of Git. Deploying this fork does not enable any automation automatically.

The workflow publishes to a dedicated `listmonk-automations` package using `GITHUB_TOKEN`. GitHub grants the creating workflow repository package access automatically. A package originally created with a personal login may require an explicit Actions access grant in its package settings; source labels alone do not grant this access. Existing deployments pinned to a previous package digest continue to use that image.
