<template>
  <section class="automations">
    <header class="page-header"><h1 class="title is-4">Automations</h1></header>
    <p class="notification is-info is-light">
      Send a welcome email after a subscriber confirms a double opt-in subscription.
      Each automation runs once per subscriber. Existing contacts and imports are not enrolled.
    </p>
    <b-tabs v-model="tab">
      <b-tab-item label="Welcome automations">
        <b-button type="is-primary" @click="newRule">New automation</b-button>
        <b-table :data="rules" :loading="busy" class="mt-4">
          <b-table-column v-slot="p" label="Name"><a href="#" @click.prevent="editRule(p.row)">{{ p.row.name }}</a></b-table-column>
          <b-table-column v-slot="p" label="List">{{ p.row.list_name }}</b-table-column>
          <b-table-column v-slot="p" label="Email template">{{ p.row.template_name }}</b-table-column>
          <b-table-column v-slot="p" label="Delay">{{ p.row.delay_seconds }} seconds</b-table-column>
          <b-table-column v-slot="p" label="Status">{{ p.row.enabled ? 'Active' : 'Paused' }}</b-table-column>
        </b-table>
        <form v-if="rule" class="box" @submit.prevent="saveRule">
          <h2 class="title is-5">{{ rule.id ? 'Edit automation' : 'New automation' }}</h2>
          <b-field label="Name"><b-input v-model="rule.name" required maxlength="200" /></b-field>
          <b-field label="Trigger"><b-input value="Subscription confirmed" disabled /></b-field>
          <b-field label="Double opt-in list">
<b-select v-model.number="rule.list_id" required expanded>
            <option v-for="l in optinLists" :key="l.id" :value="l.id">{{ l.name }}</option>
          </b-select>
</b-field>
          <b-field label="Welcome email (transactional template)">
            <b-select v-model.number="rule.template_id" required expanded>
              <option v-for="t in txTemplates" :key="t.id" :value="t.id">{{ t.name }}</option>
            </b-select>
          </b-field>
          <p class="help mb-3">Create the email in Campaigns → Templates. Include a link to <code v-pre>{{ .Tx.Data.unsubscribe_url }}</code>.</p>
          <b-field label="Delay in seconds"><b-input v-model.number="rule.delay_seconds" type="number" min="0" max="2592000" required /></b-field>
          <b-field label="From email (optional)"><b-input v-model="rule.from_email" placeholder="Use the default sender" /></b-field>
          <b-field label="Template data (JSON)"><b-input v-model="ruleData" type="textarea" /></b-field>
          <b-field><b-switch v-model="rule.enabled">Enabled</b-switch></b-field>
          <p class="help mb-3">
            Pausing stops enrollment and holds pending jobs. New confirmations after enabling are eligible;
            past confirmations are not backfilled. Changes apply to future enrollments.
          </p>
          <b-button type="is-primary" native-type="submit" :loading="busy">Save automation</b-button>
          <b-button @click="rule = null">Close</b-button>
        </form>
      </b-tab-item>
      <b-tab-item label="Confirmation emails">
        <b-field label="Double opt-in list">
<b-select v-model.number="confirmation.list_id" expanded @input="selectConfirmation">
          <option v-for="l in optinLists" :key="l.id" :value="l.id">{{ l.name }}</option>
        </b-select>
</b-field>
        <form @submit.prevent="saveConfirmation">
          <b-field label="Subject"><b-input v-model="confirmation.subject" required maxlength="500" /></b-field>
          <b-field label="HTML body" />
          <p class="help mb-3">Use Subscriber fields, .OptinURL for the confirmation button, and .UnsubURL for preferences. The button must link to .OptinURL.</p>
          <code-editor v-model="confirmation.body" lang="html" />
          <b-field class="mt-3"><b-switch v-model="confirmation.enabled">Use this confirmation email for this list</b-switch></b-field>
          <b-button type="is-primary" native-type="submit" :loading="busy" :disabled="!confirmation.list_id">Save confirmation email</b-button>
          <b-button @click="previewConfirmation">Preview</b-button>
          <b-field label="Send a test (uses sample links)" class="mt-4" grouped>
            <b-input v-model="testRecipient" type="email" expanded placeholder="you@example.com" aria-label="Test recipient" />
            <b-button @click="testConfirmation" :disabled="!testRecipient">Send test</b-button>
          </b-field>
        </form>
        <div v-if="preview" class="box mt-4">
          <h3 class="title is-6">{{ preview.subject }}</h3>
          <iframe title="Confirmation email preview" :srcdoc="preview.body" sandbox="" class="email-preview" />
        </div>
      </b-tab-item>
      <b-tab-item label="Delivery history">
        <b-button @click="refresh">Refresh</b-button>
        <p class="help mt-3">Sent means accepted by SMTP, not confirmed delivery to an inbox. Uncertain sends require review before a manual retry.</p>
        <b-table :data="jobs" detailed detail-key="id" class="mt-3">
          <b-table-column v-slot="p" label="ID">{{ p.row.id }}</b-table-column>
          <b-table-column v-slot="p" label="Automation">{{ p.row.automation_name }}</b-table-column>
          <b-table-column v-slot="p" label="Subscriber">{{ p.row.email }}</b-table-column>
          <b-table-column v-slot="p" label="Status"><b-tag>{{ p.row.status }}</b-tag></b-table-column>
          <b-table-column v-slot="p" label="Attempts">{{ p.row.attempts }}</b-table-column>
          <b-table-column v-slot="p" label="Scheduled">{{ date(p.row.scheduled_at) }}</b-table-column>
          <b-table-column v-slot="p" label="Actions">
            <b-button v-if="['pending', 'retry'].includes(p.row.status)" size="is-small" @click="jobAction(p.row, 'cancel')">Cancel</b-button>
            <b-button v-if="['failed', 'uncertain', 'cancelled'].includes(p.row.status)" size="is-small" @click="jobAction(p.row, 'retry')">Review and retry</b-button>
          </b-table-column>
          <template #detail="p">
            <p>{{ p.row.last_error }}</p>
            <ul><li v-for="h in p.row.history" :key="h.id">{{ date(h.created_at) }} — {{ h.status }} {{ h.error }}</li></ul>
          </template>
        </b-table>
      </b-tab-item>
    </b-tabs>
  </section>
</template>
<script>
import CodeEditor from '../components/CodeEditor.vue';

const defaultConfirmation = () => ({
  list_id: 0,
  subject: 'Confirm your subscription',
  body: [
    '<!DOCTYPE html><html><body><h1>Confirm your subscription</h1>',
    '<p>Please confirm that you want to subscribe.</p><p><a href="{{ .OptinURL }}">Confirm subscription</a></p>',
    '<p>If you did not request this, ignore this email.</p><a href="{{ .UnsubURL }}?manage=true">Preferences</a></body></html>',
  ].join(''),
  enabled: false,
});

export default {
  components: { CodeEditor },
  data() {
    return {
      tab: 0,
      rules: [],
      jobs: [],
      lists: [],
      templates: [],
      confirmations: [],
      rule: null,
      ruleData: '{}',
      confirmation: defaultConfirmation(),
      preview: null,
      testRecipient: '',
      busy: false,
    };
  },
  computed: {
    optinLists() { return this.lists.filter((l) => l.optin === 'double' && l.status === 'active'); },
    txTemplates() { return this.templates.filter((t) => t.type === 'tx'); },
  },
  methods: {
    date(value) { return value ? new Date(value).toLocaleString() : ''; },
    async refresh() {
      this.busy = true;
      try {
        const [rules, jobs, lists, templates, confirmations] = await Promise.all([
          this.$api.getAutomations(), this.$api.getAutomationJobs(), this.$api.getLists(),
          this.$api.getTemplates(), this.$api.getConfirmationTemplates(),
        ]);
        this.rules = rules;
        this.jobs = jobs;
        this.lists = lists.results || lists;
        this.templates = templates;
        this.confirmations = confirmations;
      } finally { this.busy = false; }
    },
    newRule() {
      this.rule = {
        name: '', list_id: 0, template_id: 0, delay_seconds: 0, from_email: '', enabled: false,
      };
      this.ruleData = '{}';
    },
    editRule(rule) { this.rule = { ...rule }; this.ruleData = JSON.stringify(rule.data || {}, null, 2); },
    async saveRule() {
      let data;
      try { data = JSON.parse(this.ruleData); } catch (e) { this.$utils.toast('Template data must be valid JSON'); return; }
      this.busy = true;
      try {
        await this.$api.saveAutomation({ ...this.rule, data });
        this.rule = null;
        this.$utils.toast('Automation saved');
        await this.refresh();
      } finally { this.busy = false; }
    },
    selectConfirmation(id) {
      this.confirmation = { ...(this.confirmations.find((v) => v.list_id === id) || defaultConfirmation()), list_id: id };
      this.preview = null;
    },
    async saveConfirmation() {
      await this.$api.saveConfirmationTemplate(this.confirmation);
      this.$utils.toast('Confirmation email saved');
      await this.refresh();
    },
    async previewConfirmation() { this.preview = await this.$api.previewConfirmationTemplate(this.confirmation); },
    async testConfirmation() {
      await this.$api.testConfirmationTemplate({ ...this.confirmation, to: this.testRecipient });
      this.$utils.toast('Test email accepted by SMTP');
    },
    async jobAction(job, action) {
      if (action === 'retry' && !window.confirm('A previous send may already have reached SMTP. Retrying can send a duplicate. Continue?')) return;
      await this.$api.automationJobAction(job.id, { action, acknowledge_duplicate: action === 'retry' });
      await this.refresh();
    },
  },
  mounted() { this.refresh(); },
};
</script>
<style scoped>
.email-preview { width: 100%; min-height: 600px; border: 0; }
.code-editor { min-height: 300px; max-height: 600px; overflow: auto; border: 1px solid #ddd; }
</style>
