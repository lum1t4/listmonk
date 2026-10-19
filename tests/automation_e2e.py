#!/usr/bin/env python3
"""Run against a disposable local instance and Mailpit. Never use production URLs.
LISTMONK_TEST_URL defaults localhost:19000; LISTMONK_TEST_TOKEN is a superadmin API token.
MAILPIT_TEST_URL defaults localhost:18025. A fresh test database is recommended.
"""
import base64
import html
import json
import os
import re
import time
import urllib.error
import urllib.parse
import urllib.request

BASE = os.environ.get('LISTMONK_TEST_URL', 'http://localhost:19000')
MAIL = os.environ.get('MAILPIT_TEST_URL', 'http://localhost:18025')
assert urllib.parse.urlparse(BASE).hostname in ('localhost', '127.0.0.1'), 'Local tests only'
assert urllib.parse.urlparse(MAIL).hostname in ('localhost', '127.0.0.1'), 'Local SMTP sink only'
TOKEN = os.environ['LISTMONK_TEST_TOKEN']
AUTH = 'Basic ' + base64.b64encode(('integration-test:' + TOKEN).encode()).decode()

def request(path, data=None, method=None, auth=True, expected=200):
    headers = {'Authorization': AUTH} if auth else {}
    if data is not None:
        data = json.dumps(data).encode()
        headers['Content-Type'] = 'application/json'
    req = urllib.request.Request(BASE + path, data=data, headers=headers, method=method)
    try:
        resp = urllib.request.urlopen(req, timeout=15)
        code, body = resp.status, resp.read().decode()
    except urllib.error.HTTPError as e:
        code, body = e.code, e.read().decode()
    assert code == expected, (path, code, expected, body[:600])
    try:
        return json.loads(body).get('data')
    except (ValueError, AttributeError):
        return body

def messages():
    return json.load(urllib.request.urlopen(MAIL + '/api/v1/messages'))['messages'] or []

def wait_for(fn, message):
    for _ in range(100):
        result = fn()
        if result:
            return result
        time.sleep(.2)
    raise AssertionError(message)

def mail_body(subject):
    item = wait_for(lambda: next((m for m in messages() if m['Subject'] == subject), None), subject)
    return json.load(urllib.request.urlopen(MAIL + '/api/v1/message/' + item['ID']))

stamp = str(time.time_ns())
request('/api/automations', auth=False, expected=403)
role = request('/api/roles/users', {'name': 'Restricted ' + stamp, 'type': 'user', 'permissions': ['lists:get_all']}, 'POST')
restricted = request('/api/users', {'username': 'restricted-' + stamp, 'type': 'api', 'user_role_id': role['id'], 'status': 'enabled'}, 'POST')
original_auth = AUTH
AUTH = 'Basic ' + base64.b64encode((restricted['username'] + ':' + restricted['password']).encode()).decode()
for endpoint in ('/api/automations', '/api/automation-jobs', '/api/confirmation-templates'):
    request(endpoint, expected=403)
AUTH = original_auth

lst = request('/api/lists', {'name': 'Automation test ' + stamp, 'type': 'public', 'optin': 'double', 'tags': []}, 'POST')
lid, luuid = lst['id'], lst['uuid']
welcome_subject = 'Welcome ' + stamp
welcome = request('/api/templates', {'name': 'Generic welcome ' + stamp, 'type': 'tx', 'subject': welcome_subject, 'body': '<html><body><h1>Welcome {{ .Subscriber.Name }}</h1><p>{{ .Tx.Data.offer }}</p><a href="{{ .Tx.Data.unsubscribe_url }}">Unsubscribe</a></body></html>'}, 'POST')
rule = {'name': 'On confirmed ' + stamp, 'list_id': lid, 'template_id': welcome['id'], 'enabled': True, 'delay_seconds': 0, 'data': {'offer': 'A generic offer'}}
for invalid in ({**rule, 'delay_seconds': -1}, {**rule, 'list_id': 9999999}, {**rule, 'from_email': 'bad\r\nBcc: bad@example.test'}):
    request('/api/automations', invalid, 'POST', expected=400)
r = request('/api/automations', rule, 'POST')
assert any(x['id'] == r['id'] for x in request('/api/automations'))
confirmation_subject = 'Please confirm ' + stamp
config = {'subject': confirmation_subject, 'body': '<html><body><h1>Confirm subscription</h1><p>{{ .Subscriber.Name }}</p><a href="{{ .OptinURL }}">Confirm</a><a href="{{ .UnsubURL }}">Preferences</a></body></html>', 'enabled': True}
request('/api/confirmation-templates/' + str(lid), {**config, 'body': '<!-- {{ .OptinURL }} -->'}, 'PUT', expected=400)
request('/api/confirmation-templates/' + str(lid), config, 'PUT')
preview = request('/api/confirmation-templates/preview', config, 'POST')
assert 'https://example.invalid/confirm' in preview['body']
request('/api/confirmation-templates/test', {**config, 'to': 'preview@example.test'}, 'POST')
test = mail_body('[TEST] ' + confirmation_subject)
assert 'https://example.invalid/confirm' in test['HTML']
assert not request('/api/automation-jobs'), 'Preview enrolled a subscriber'
email = 'subscriber-' + stamp + '@example.test'
sub = request('/api/public/subscription', {'email': email, 'name': '<Sample>', 'list_uuids': [luuid]}, 'POST', auth=False)
assert sub['has_optin'] is True
confirm = mail_body(confirmation_subject)
assert '&lt;Sample&gt;' in confirm['HTML']
assert not any(m['Subject'] == welcome_subject for m in messages()), 'Welcome before confirmation'
url = html.unescape(next(u for u in re.findall(r'href="([^"]+)"', confirm['HTML']) if '/subscription/optin/' in u))
path = url.removeprefix(BASE)
request(path, auth=False)
assert not request('/api/automation-jobs'), 'Opening confirmation page enrolled without confirmation'
confirm_path = path + ('&' if '?' in path else '?') + 'confirm=true'
request(confirm_path, auth=False)
welcome_mail = mail_body(welcome_subject)
assert 'A generic offer' in welcome_mail['HTML'] and '&lt;Sample&gt;' in welcome_mail['HTML']
job = wait_for(lambda: next((x for x in request('/api/automation-jobs') if x['automation_id'] == r['id'] and x['status'] == 'sent'), None), 'Job not recorded')
assert job['attempts'] == 1 and len(job['history']) == 1
request(confirm_path, auth=False)
time.sleep(2.5)
assert len([m for m in messages() if m['Subject'] == welcome_subject]) == 1, 'Duplicate welcome'
request('/api/automation-jobs/' + str(job['id']) + '/actions', {'action': 'retry'}, 'POST', expected=400)
request('/api/automation-jobs/' + str(job['id']) + '/actions', {'action': 'retry', 'acknowledge_duplicate': True}, 'POST', expected=409)
# Preconfirmed API contacts must not enroll or receive a welcome.
request('/api/subscribers', {'email': 'import-' + stamp + '@example.test', 'name': 'Imported', 'status': 'enabled', 'lists': [lid], 'preconfirm_subscriptions': True}, 'POST')
time.sleep(2.5)
assert len([m for m in messages() if m['Subject'] == welcome_subject]) == 1
# The native welcome unsubscribe link must remain functional.
unsub = html.unescape(next(u for u in re.findall(r'href="([^"]+)"', welcome_mail['HTML']) if '/subscription/' in u))
request(unsub.removeprefix(BASE), auth=False)
# Static SPA route is available in the built container.
assert 'html' in request('/admin/automations', auth=False).lower()
print('PASS: real HTTP form → custom opt-in → explicit confirmation → welcome SMTP → durable history; duplicate clicks, preview isolation, imports, validation, unsubscribe, and admin bundle')
