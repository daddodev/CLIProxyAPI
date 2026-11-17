import assert from "node:assert/strict"
import { readFileSync } from "node:fs"
import test from "node:test"
import vm from "node:vm"

const html = readFileSync(new URL("./usage.html", import.meta.url), "utf8")
function section(start, end) {
    const from = html.indexOf(start)
    const to = html.indexOf(end, from)
    assert.ok(from >= 0 && to > from, `Missing script section: ${start}`)
    return html.slice(from, to)
}

// Exercise the embedded page's reset flow without a DOM or third-party runner.
const resetScript = [
    section("const $ =", "const round ="),
    section("// Claims whose outcome is unknown", "// ---------- API ----------"),
    section("const OUTCOMES =", "function providerHtml("),
    section("// ---------- Reset flow ----------", "// ---------- Edit mode ----------"),
].join("\n")

function harness({ storage = new Map(), locks = { request: async (_name, pick) => pick() }, randomUUID = () => "new-request-id" } = {}) {
    const calls = []
    const element = { addEventListener() {}, focus() {} }
    const context = vm.createContext({
        KEY_STORAGE: "key",
        API: "/v8/management/observability/usage",
        DEMO: false,
        loadPrefs: () => ({}),
        localStorage: { getItem: (key) => storage.get(key), setItem: (key, value) => storage.set(key, value) },
        navigator: { locks },
        window: { addEventListener() {} },
        document: { querySelector: () => element },
        resetPop: element,
        CSS: { escape: (value) => value },
        nf: new Intl.NumberFormat("en"),
        absShort: (value) => value,
        renderLimits() {},
        loadLimits: async () => {},
        api: async (path, init) => {
            calls.push({ path, body: JSON.parse(init.body) })
            return { outcome: "reset" }
        },
        crypto: { randomUUID },
    })
    vm.runInContext(resetScript, context)
    const page = vm.runInContext("({ state, resetChoice, resetOptions, resetButtonHtml, resetPopHtml, onLimitsClick, loadPending })", context)
    const account = {
        auth_index: "account-1", label: "person@example.com", disabled: false,
        resets_observed_at: new Date().toISOString(), resets_stale: false, resets_error: "",
        resets: {
            available: 3, next_credit_id: "session-grant",
            options: [
                { credit_id: "session-grant", reset_type: "session", clears: ["five_hour"], available: 2, expires_at: new Date(Date.now() + 3600000).toISOString(), label: "Session credit" },
                { credit_id: "weekly-grant", reset_type: "weekly", clears: ["seven_day"], available: 1, label: "Weekly credit" },
            ],
        },
    }
    const provider = { provider: "claude", label: "Claude", accounts: [account] }
    page.state.limits = { providers: [provider] }
    const click = (action, credit) => page.onLimitsClick({ target: { closest: () => ({ dataset: { action, id: account.auth_index, credit } }) } })
    const render = () => page.resetPopHtml(provider, [{ a: account, color: "#cc5824" }])
    return { page, account, storage, context, calls, click, render }
}

// Let the lock, API and refresh promises finish without wall-clock delays.
const settle = () => new Promise(setImmediate)

test("shows each reset type, scope, count and expiry, with escaped provider labels", () => {
    const h = harness()
    h.account.resets.options[1].label = '<img src=x onerror="alert(1)">'
    const markup = h.render()
    assert.match(markup, /5-hour reset/)
    assert.match(markup, /Weekly reset/)
    assert.match(markup, /Resets: 5-hour\./)
    assert.match(markup, /Resets: weekly\./)
    assert.match(markup, /2 left · expires/)
    assert.match(markup, /data-credit="weekly-grant"/)
    assert.match(markup, /&lt;img/)
    assert.doesNotMatch(markup, /<img/)
    assert.equal(h.page.resetChoice({}).detail, "Affected limits not reported.")
})

test("redeems the confirmed weekly grant instead of the suggested next session grant", async () => {
    const h = harness()
    h.click("reset-open", "weekly-grant")
    assert.equal(h.page.state.resetUi.get("account-1").creditId, "weekly-grant")
    assert.match(h.render(), /Use <b>Weekly reset<\/b>/)
    h.account.resets.options.reverse()
    h.account.resets.next_credit_id = "different-next-grant"
    h.click("reset-confirm")
    h.click("reset-confirm")
    await settle()
    assert.equal(h.calls.length, 1)
    assert.deepEqual(h.calls[0].body, { auth_index: "account-1", credit_id: "weekly-grant", request_id: "new-request-id" })
    assert.equal(h.page.loadPending().size, 0)
})

test("never substitutes another credit if the confirmed grant disappears or expires", async () => {
    for (const change of [
        (a) => { a.resets.options.pop() },
        (a) => { a.resets.options[1].expires_at = "2000-01-01T00:00:00Z" },
        (a) => { a.disabled = true },
    ]) {
        const h = harness()
        h.click("reset-open", "weekly-grant")
        change(h.account)
        h.click("reset-confirm")
        await settle()
        assert.equal(h.calls.length, 0)
        assert.equal(h.page.state.resetUi.get("account-1").code, "not_found")
    }
})

test("canceling a selection sends no redemption", async () => {
    const h = harness()
    h.click("reset-open", "weekly-grant")
    h.click("reset-cancel", "weekly-grant")
    await settle()
    assert.equal(h.calls.length, 0)
    assert.equal(h.page.state.resetUi.size, 0)
})

test("reset summaries distinguish unknown, fresh zero, stale inventory and read errors", () => {
    const h = harness()
    const summary = () => h.page.resetButtonHtml({ provider: "claude" }, [{ a: h.account }])
    assert.match(summary(), />3 resets</)
    h.account.resets_stale = true
    assert.match(summary(), /3 resets · stale/)
    assert.match(h.render(), /3 credits last known · stale/)
    h.account.resets_error = "Credit endpoint failed"
    assert.match(summary(), /3 resets · read error/)
    assert.match(h.render(), /Credit read error/)
    h.account.resets = null
    assert.match(summary(), /Reset read error/)
    h.account.resets_error = ""
    assert.match(summary(), /Resets unknown/)
    assert.match(h.render(), /Credits unknown/)
    h.account.resets = { available: 0, options: [] }
    h.account.resets_stale = false
    assert.match(summary(), /No resets/)
    assert.match(h.render(), /No credits/)
})

test("scoped windows show their own stale timestamp and accessible status", () => {
    const h = harness()
    Object.assign(h.context, { reserveOf: () => 0, paceAt: () => null, fmtPct: (value) => `${value}%`, round: (value) => value })
    vm.runInContext(section("const winKey =", "// Reserves exist") + section("function cellHtml(", "function accountHtml("), h.context)
    const cell = vm.runInContext("cellHtml", h.context)
    const markup = cell({}, { kind: "other", label: "Weekly · Fable", used_percent: 71, stale: true, observed_at: "original-probe-time" })
    assert.match(markup, /Weekly · Fable/)
    assert.match(markup, /role="status">Stale · read original-probe-time/)
    assert.match(markup, /aria-label="Weekly · Fable: 29% left, Stale/)
    assert.doesNotMatch(cell({}, { kind: "other", label: "Weekly · Fable", used_percent: 71, stale: false }), /Stale/)
})

test("stale credit inventory stays readable but cannot start a new claim", async () => {
    for (const change of [
        (a) => { a.resets_stale = true },
        (a) => { a.resets_error = "Credit endpoint failed" },
        (a) => { a.resets_observed_at = new Date(Date.now() - 6 * 60000).toISOString() },
        (a) => { a.resets_observed_at = null },
    ]) {
        const h = harness()
        change(h.account)
        assert.match(h.render(), /data-action="reset-open"[^>]* disabled/)
        assert.match(h.render(), /Weekly reset/)
        assert.equal(h.page.resetOptions(h.account).length, 0)
        h.click("reset-open", "weekly-grant")
        await settle()
        assert.equal(h.calls.length, 0)
        assert.equal(h.page.state.resetUi.size, 0)
    }
})

test("credits aging after confirmation block spending but not an original unresolved retry", async () => {
    const h = harness()
    h.click("reset-open", "weekly-grant")
    h.account.resets_stale = true
    assert.match(h.render(), /data-action="reset-confirm"[^>]* disabled/)
    h.click("reset-confirm")
    await settle()
    assert.equal(h.calls.length, 0)
    assert.equal(h.page.state.resetUi.get("account-1").code, "stale_credits")

    h.storage.set("cpa-usage-pending-claims", JSON.stringify({
        "account-1": { creditId: "weekly-grant", requestId: "original-request", at: Date.now(), choice: { label: "Weekly reset", detail: "Resets: weekly." } },
    }))
    h.account.resets = null
    h.account.resets_error = "Credit endpoint failed"
    h.click("reset-open", "session-grant")
    assert.match(h.render(), /Retry same credit/)
    h.click("reset-retry")
    await settle()
    assert.deepEqual(h.calls[0].body, { auth_index: "account-1", credit_id: "weekly-grant", request_id: "original-request" })
})

test("credits becoming stale while waiting for the cross-tab lock cannot create a claim", async () => {
    const lock = deferred()
    const h = harness({ locks: { request: (_name, change) => lock.promise.then(change) } })
    h.click("reset-open", "weekly-grant")
    h.click("reset-confirm")
    h.account.resets_stale = true
    lock.resolve()
    await settle()
    assert.equal(h.calls.length, 0)
    assert.equal(h.page.loadPending().size, 0)
    assert.equal(h.page.state.resetUi.get("account-1").code, "stale_credits")
})

test("unconfirmed retries retain the selected type, credit and request ID after reload", async () => {
    const h = harness()
    h.context.api = async (_path, init) => {
        h.calls.push(JSON.parse(init.body))
        throw { code: "unconfirmed", message: "Response lost" }
    }
    h.click("reset-open", "weekly-grant")
    h.click("reset-confirm")
    await settle()
    const claim = h.page.loadPending().get("account-1")
    assert.equal(claim.creditId, "weekly-grant")
    assert.equal(claim.choice.label, "Weekly reset")
    h.page.state.resetUi.clear()
    h.account.resets.options.pop()
    h.click("reset-open", "session-grant")
    assert.match(h.render(), /Weekly reset/)
    assert.match(h.render(), /Retry same credit/)
    h.click("reset-retry")
    await settle()
    assert.equal(h.calls.length, 2)
    assert.deepEqual(h.calls[1], h.calls[0])
})

test("a pending claim from another tab must be reviewed before a different grant is spent", async () => {
    const h = harness()
    h.click("reset-open", "weekly-grant")
    h.storage.set("cpa-usage-pending-claims", JSON.stringify({
        "account-1": { creditId: "session-grant", requestId: "previous-request", at: Date.now(), choice: { label: "5-hour reset", detail: "Resets: 5-hour." } },
    }))
    h.click("reset-confirm")
    await settle()
    assert.equal(h.calls.length, 0)
    assert.equal(h.page.state.resetUi.get("account-1").creditId, "session-grant")
    assert.match(h.render(), /Retry same credit/)
})

function sharedTabs() {
    const storage = new Map()
    let queue = Promise.resolve()
    let request = 0
    const locks = {
        request(_name, change) {
            const result = queue.then(change)
            queue = result.catch(() => {})
            return result
        },
    }
    const options = { storage, locks, randomUUID: () => `request-${++request}` }
    return { options, tab: () => harness(options) }
}

function deferred() {
    let resolve, reject
    const promise = new Promise((yes, no) => { resolve = yes; reject = no })
    return { promise, resolve, reject }
}

test("a delayed tab cannot clear a newer unresolved claim", async () => {
    const tabs = sharedTabs()
    const a = tabs.tab()
    const b = tabs.tab()
    const qA = deferred(), qB = deferred(), rA = deferred()
    a.context.api = (_path, init) => {
        a.calls.push(JSON.parse(init.body))
        return a.calls.length === 1 ? qA.promise : rA.promise
    }
    b.context.api = (_path, init) => {
        b.calls.push(JSON.parse(init.body))
        return qB.promise
    }

    a.click("reset-open", "session-grant")
    a.click("reset-confirm")
    await settle()
    b.click("reset-open", "session-grant")
    b.click("reset-retry")
    await settle()
    assert.deepEqual(a.calls[0], b.calls[0])

    qA.resolve({ outcome: "reset" })
    await settle()
    a.click("reset-open", "weekly-grant")
    a.click("reset-confirm")
    await settle()
    const newer = a.page.loadPending().get("account-1")
    assert.equal(newer.requestId, "request-2")
    assert.equal(newer.creditId, "weekly-grant")

    qB.resolve({ outcome: "reset" })
    await settle()
    assert.equal(b.page.loadPending().get("account-1").requestId, newer.requestId)
    assert.equal(b.page.state.resetUi.get("account-1").creditId, newer.creditId)

    rA.reject({ code: "unconfirmed", message: "Response lost" })
    await settle()
    const reloaded = tabs.tab()
    reloaded.click("reset-retry")
    await settle()
    assert.deepEqual(reloaded.calls[0].body, a.calls[1])
})

test("stale retries and dismissals cannot replace or hide a different pending claim", async () => {
    for (const action of ["reset-retry", "reset-dismiss"]) {
        const tabs = sharedTabs()
        const h = tabs.tab()
        h.page.state.resetUi.set("account-1", { phase: "done", code: "unconfirmed", creditId: "session-grant", requestId: "old-request" })
        h.storage.set("cpa-usage-pending-claims", JSON.stringify({
            "account-1": { creditId: "weekly-grant", requestId: "newer-request", at: Date.now() },
        }))
        h.click(action)
        await settle()
        assert.equal(h.calls.length, 0)
        const claim = h.page.loadPending().get("account-1")
        assert.equal(claim.requestId, "newer-request")
        assert.equal(claim.dismissed, undefined)
        assert.equal(h.page.state.resetUi.get("account-1").creditId, "weekly-grant")
    }
})

test("unresolved claims older than 24 hours keep their request ID across reloads", async () => {
    const storage = new Map([["cpa-usage-pending-claims", JSON.stringify({
        "account-1": { creditId: "session-grant", requestId: "week-old-request", at: Date.now() - 7 * 86400000 },
    })]])
    const h = harness({ storage })
    assert.equal(h.page.loadPending().get("account-1").requestId, "week-old-request")
    h.click("reset-open", "weekly-grant")
    assert.equal(h.page.state.resetUi.get("account-1").creditId, "session-grant")
    h.click("reset-retry")
    await settle()
    assert.equal(h.calls[0].body.request_id, "week-old-request")
    assert.equal(h.calls[0].body.credit_id, "session-grant")
})

for (const code of ["signed_out", "rate_limited"]) {
    test(`a retry's ${code} error cannot settle an earlier unknown redemption`, async () => {
        const h = harness({ randomUUID: () => "original-request" })
        h.context.api = async (_path, init) => {
            h.calls.push(JSON.parse(init.body))
            throw { code: h.calls.length === 1 ? "unconfirmed" : code, message: "Retry failed" }
        }
        h.click("reset-open", "session-grant")
        h.click("reset-confirm")
        await settle()
        h.click("reset-retry")
        await settle()
        assert.equal(h.page.loadPending().get("account-1").requestId, "original-request")
        assert.match(h.render(), /Retry same credit/)

        const reloaded = harness({ storage: h.storage })
        reloaded.click("reset-open", "weekly-grant")
        assert.equal(reloaded.page.state.resetUi.get("account-1").creditId, "session-grant")
        reloaded.click("reset-retry")
        await settle()
        assert.equal(reloaded.calls[0].body.request_id, "original-request")
        assert.equal(reloaded.calls[0].body.credit_id, "session-grant")
        assert.equal(reloaded.page.loadPending().size, 0)
    })
}

test("without Web Locks, reset options stay readable but spending and pending writes are blocked", async () => {
    const h = harness({ locks: null })
    assert.match(h.render(), /use HTTPS or localhost/)
    assert.match(h.render(), /data-action="reset-open"[^>]* disabled/)
    h.click("reset-open", "weekly-grant")
    assert.equal(h.page.state.resetUi.size, 0)

    const pending = JSON.stringify({ "account-1": { creditId: "session-grant", requestId: "unresolved-request" } })
    h.storage.set("cpa-usage-pending-claims", pending)
    h.page.state.resetUi.set("account-1", { phase: "confirm", creditId: "weekly-grant", choice: { label: "Weekly reset", detail: "Resets: weekly." } })
    h.click("reset-confirm")
    h.page.state.resetUi.set("account-1", { phase: "done", code: "unconfirmed", creditId: "session-grant", requestId: "unresolved-request" })
    assert.match(h.render(), /data-action="reset-retry"[^>]* disabled/)
    h.click("reset-retry")
    await settle()
    assert.equal(h.calls.length, 0)
    assert.equal(h.storage.get("cpa-usage-pending-claims"), pending)
    await h.click("reset-dismiss")
    assert.equal(h.storage.get("cpa-usage-pending-claims"), pending)
})
