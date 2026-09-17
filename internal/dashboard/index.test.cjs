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
    const context = {
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
