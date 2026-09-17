const assert = require('node:assert/strict');
const { readFileSync } = require('node:fs');
const { join } = require('node:path');
const { test } = require('node:test');
const { runInNewContext } = require('node:vm');

test('auto-downtime toggles manual entry without waiting for status polling', () => {
    const html = readFileSync(join(__dirname, 'index.html'), 'utf8');
    const start = html.indexOf("var autoDowntimeChk = document.getElementById('auto_downtime');");
    const end = html.indexOf("    ['dest_ip', 'vm_ip'].forEach", start);
    assert.notEqual(start, -1);
    assert.notEqual(end, -1);
    const source = html.slice(start, end);
    const checkbox = new EventTarget();
    checkbox.checked = false;
    const classes = new Set();
    const downtime = {
        disabled: false,
        classList: {
            toggle(name, enabled) {
                if (enabled) classes.add(name);
                else classes.delete(name);
            }
        }
    };
    let cleared = 0;
    const context = {
        clearFieldState(field) {
            assert.equal(field, downtime);
            cleared++;
        },
        document: {
            getElementById(id) {
                assert.ok(id === 'auto_downtime' || id === 'downtime');
                return id === 'auto_downtime' ? checkbox : downtime;
            }
        }
    };
    runInNewContext(source, context, { timeout: 1000 });
    function expectDisabled(expected) {
        assert.equal(downtime.disabled, expected);
        assert.equal(classes.has('opacity-50'), expected);
        assert.equal(classes.has('cursor-not-allowed'), expected);
    }
    expectDisabled(false);
    checkbox.checked = true;
    checkbox.dispatchEvent(new Event('change'));
    expectDisabled(true);
    assert.equal(cleared, 1);
    checkbox.checked = false;
    checkbox.dispatchEvent(new Event('change'));
    expectDisabled(false);
    context.syncDowntimeEnabled(true);
    expectDisabled(true);
    context.syncDowntimeEnabled(false);
    expectDisabled(false);
    checkbox.checked = true;
    context.syncDowntimeEnabled(false);
    expectDisabled(true);
});

for (const [value, automatic, badInput, allowed] of [
    ['0', true, false, true],
    ['60001', true, false, true],
    ['', true, true, true],
    ['', false, false, true],
    ['1', false, false, true],
    ['60000', false, false, true],
    ['0', false, false, false],
    ['60001', false, false, false],
    ['25.5', false, false, false],
    ['1e2', false, false, false],
    ['', false, true, false]
]) {
    test(`migration downtime ${JSON.stringify(value)}, automatic=${automatic}, badInput=${badInput}`, () => {
        const html = readFileSync(join(__dirname, 'index.html'), 'utf8');
        const start = html.indexOf("    migrationForm.addEventListener('submit'");
        const end = html.indexOf("    var loadgenType =", start);
        assert.ok(start >= 0 && end > start);
        const fields = new Map();
        const getField = id => {
            if (!fields.has(id)) fields.set(id, {
                id, value: '', disabled: false,
                focus() {}, setAttribute() {}, removeAttribute() {}
            });
            return fields.get(id);
        };
        getField('source_pod_name').value = 'source';
        getField('downtime').value = value;
        getField('downtime').validity = { badInput };
        getField('auto_downtime').checked = automatic;
        const invalid = [];
        const requests = [];
        let submit;
        const form = {
            addEventListener(event, handler) { assert.equal(event, 'submit'); submit = handler; },
            querySelectorAll() { return []; },
            setAttribute() {}, removeAttribute() {}
        };
        runInNewContext(html.slice(start, end), {
            migrationForm: form,
            document: { getElementById: getField },
            getField, syncSourcePodFields() {}, syncDestNodeField() { return 'worker-b'; },
            clearFieldState() {}, markFieldInvalid(field) { invalid.push(field.id); },
            showToast() {},
            FormData: class { constructor() { return []; } },
            URLSearchParams,
            apiCall(url) { requests.push(url); return Promise.resolve({ ok: true }); }
        }, { timeout: 1000 });
        submit({ preventDefault() {}, target: form });
        assert.deepEqual(requests, allowed ? ['/api/migrate'] : []);
        assert.deepEqual(invalid, allowed ? [] : ['downtime']);
    });
}

function statusHarness() {
    const html = readFileSync(join(__dirname, 'index.html'), 'utf8');
    const start = html.indexOf('    var fetchInFlight = false;');
    const end = html.indexOf("    document.addEventListener('visibilitychange'", start);
    assert.ok(start >= 0 && end > start);
    const elements = new Map();
    function element(id) {
        if (!elements.has(id)) elements.set(id, {
            textContent: '', style: {}, className: '',
            classList: { add() {}, remove() {} },
            setAttribute() {}, removeAttribute() {}, toggleAttribute() {},
            querySelector(selector) { return element(id + selector); },
            get parentElement() { return element(id + '-parent'); }
        });
        return elements.get(id);
    }
    let response;
    const context = {
        document: {
            hidden: false, getElementById: element,
            querySelectorAll() { return []; }
        },
        window: { addEventListener() {}, removeEventListener() {} },
        migrationForm: element('migration-form'),
        logsDiv: element('logs'),
        lastLogSeq: 0, lastPingSeq: 0, activeLogMigrationID: '', pingSamples: [],
        loadgenType: 'ICMP', loadgenTarget: '', wasMigrating: false,
        latencyChart: {
            data: { labels: [], datasets: [{ data: [] }] }, update() {}
        },
        URLSearchParams,
        fetch: async () => ({ ok: true, json: async () => response }),
        ensureChart() {}, renderProgress() {}, syncDowntimeEnabled() {}, showToast() {},
        console: { error(message, err) { throw err; } }
    };
    runInNewContext(html.slice(start, end), context, { timeout: 1000 });
    return {
        element,
        async refresh(data) {
            response = { migrating: false, loadgen_running: true, loadgen_type: 'ping', ...data };
            await context.refreshStatus();
        }
    };
}

test('latency statistics clear when successful samples leave the rolling window', async () => {
    const dashboard = statusHarness();
    await dashboard.refresh({ pings: [{ latency: 12 }] });
    assert.equal(dashboard.element('stat-avg').textContent, '12.00 ms');
    assert.equal(dashboard.element('stat-max').textContent, '12.00 ms');
    await dashboard.refresh({ pings: Array.from({ length: 500 }, () => ({ error: 'Timeout' })) });
    assert.equal(dashboard.element('stat-avg').textContent, '\u2014');
    assert.equal(dashboard.element('stat-max').textContent, '\u2014');
    assert.equal(dashboard.element('stat-lost[aria-hidden]').textContent, '\u26A0 500');
    await dashboard.refresh({ pings: [{ latency: 8 }] });
    assert.equal(dashboard.element('stat-avg').textContent, '8.00 ms');
    assert.equal(dashboard.element('stat-max').textContent, '8.00 ms');
});
