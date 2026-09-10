/**
 * Migration 0113 relabels 272 rows that claimed to be real money.
 *
 * A data migration is only as trustworthy as the state it was exercised in. The
 * fresh-migration check applies the whole chain to an empty database, where 0113
 * updates nothing and passes — the same blind spot that let 0112 reach the live
 * Sandbox broken. So this seeds the defect the migration exists for, and then
 * mutation-proves both gates: a fail-closed precondition nobody has watched
 * refuse is not known to refuse.
 *
 *   DATABASE_URL=postgres://localhost:5432/postgres node --test tests/ops/migration-0113-environment-repair.test.mjs
 */
import { execFileSync } from 'node:child_process';
import { join } from 'node:path';
import assert from 'node:assert/strict';
import { after, before, describe, it } from 'node:test';

const REPO = join(import.meta.dirname, '../..');
const ADMIN = process.env.DATABASE_URL ?? 'postgres://localhost:5432/postgres';
const MIG = join(REPO, 'db/migrations');
const FILE = join(MIG, '0113_environment_repair.sql');

/** The six tables whose writers omitted the column. */
const REPAIRED = ['payment_links', 'qr_codes', 'transfers', 'payouts', 'webhook_endpoints', 'transactions'];

let tmplName;
let url;
const created = [];

const psql = (target, sql) =>
  execFileSync('psql', [target, '-v', 'ON_ERROR_STOP=1', '-Atc', sql], {
    encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'],
  });

/** Run 0113 and report rather than throw, so a refusal is assertable. */
function run0113() {
  try {
    execFileSync('psql', [url, '-v', 'ON_ERROR_STOP=1', '-f', FILE],
      { encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] });
    return { ok: true, message: '' };
  } catch (e) {
    return { ok: false, message: `${e.stderr ?? ''}${e.stdout ?? ''}` };
  }
}

const M = '11111111-1111-4111-8111-111111111111'; // merchant
const W = '22222222-2222-4222-8222-222222222222'; // wallet
const C1 = '33333333-3333-4333-8333-333333333331'; // consumer (sender)
const C2 = '33333333-3333-4333-8333-333333333332'; // consumer (recipient)
const A1 = '44444444-4444-4444-8444-444444444441'; // ledger accounts
const A2 = '44444444-4444-4444-8444-444444444442';

/**
 * Seed one mislabelled row in each of the six tables — the exact shape of the
 * defect: correct financial content, wrong universe.
 */
function seedMislabelled() {
  psql(url, `
    INSERT INTO merchants (id, name, email) VALUES ('${M}', 'Seed', 'seed@example.test');
    INSERT INTO ledger_accounts (id, account_type, name, currency)
      VALUES ('${A1}', 'LIABILITY', 'seed available', 'AOA'),
             ('${A2}', 'LIABILITY', 'seed reserved',  'AOA');
    INSERT INTO wallets (id, merchant_id, currency, available_account_id, reserved_account_id)
      VALUES ('${W}', '${M}', 'AOA', '${A1}', '${A2}');
    INSERT INTO consumers (id, handle) VALUES ('${C1}', 'seed_sender'), ('${C2}', 'seed_recipient');

    INSERT INTO payment_links (id, slug, merchant_id, wallet_id, currency, environment)
      VALUES (gen_random_uuid(), 'seed-link', '${M}', '${W}', 'AOA', 'LIVE');
    INSERT INTO qr_codes (owner_id, owner_type, qr_type, currency, environment)
      VALUES ('${M}', 'MERCHANT', 'STATIC', 'AOA', 'LIVE');
    INSERT INTO transfers (idempotency_key, sender_id, recipient_id, amount_minor, currency, environment)
      VALUES ('seed-transfer', '${C1}', '${C2}', 1000, 'AOA', 'LIVE');
    INSERT INTO payouts (merchant_id, wallet_id, idempotency_key, status, amount_minor, currency,
                         bank_account_number, bank_code, account_holder_name, environment)
      VALUES ('${M}', '${W}', 'seed-payout', 'PENDING', 1000, 'AOA', '000', 'BAI', 'Seed', 'LIVE');
    INSERT INTO webhook_endpoints (merchant_id, url, secret, environment)
      VALUES ('${M}', 'https://example.test/hook', 'seed-secret', 'LIVE');
    INSERT INTO transactions (id, idempotency_key, transaction_type, amount_minor, currency,
                              merchant_id, wallet_id, environment)
      VALUES (gen_random_uuid(), 'seed-txn', 'PAYMENT', 1000, 'AOA', '${M}', '${W}', 'LIVE');
  `);
}

const liveRows = () =>
  Number(psql(url, `SELECT ${REPAIRED.map((t) => `(SELECT count(*) FROM ${t} WHERE environment='LIVE')`).join(' + ')}`).trim());

const sandboxRows = () =>
  Number(psql(url, `SELECT ${REPAIRED.map((t) => `(SELECT count(*) FROM ${t} WHERE environment='SANDBOX')`).join(' + ')}`).trim());

function freshCase(name) {
  url = ADMIN.replace(/\/[^/?]+(\?.*)?$/, `/${name}$1`);
  psql(ADMIN, `DROP DATABASE IF EXISTS ${name}`);
  psql(ADMIN, `CREATE DATABASE ${name} TEMPLATE ${tmplName}`);
  created.push(name);
  seedMislabelled();
}

describe('migration 0113 — environment repair', () => {
  before(() => {
    tmplName = `bz_0113_tmpl_${process.pid}`;
    const tmplUrl = ADMIN.replace(/\/[^/?]+(\?.*)?$/, `/${tmplName}$1`);
    psql(ADMIN, `DROP DATABASE IF EXISTS ${tmplName}`);
    psql(ADMIN, `CREATE DATABASE ${tmplName}`);
    // Chain to 0112 only; 0113 is the subject and is run explicitly per case.
    execFileSync('sqlx', ['migrate', 'run', '--source', MIG, '--target-version', '112', '-D', tmplUrl],
      { cwd: REPO, stdio: ['ignore', 'pipe', 'inherit'] });
  });

  after(() => {
    for (const d of created) { try { psql(ADMIN, `DROP DATABASE IF EXISTS ${d}`); } catch { /* best effort */ } }
    try { psql(ADMIN, `DROP DATABASE IF EXISTS ${tmplName}`); } catch { /* best effort */ }
  });

  it('relabels every mislabelled row and leaves none claiming LIVE', () => {
    freshCase('bz_0113_repair');
    assert.equal(liveRows(), REPAIRED.length, 'precondition: one mislabelled row per table');
    const r = run0113();
    assert.ok(r.ok, `0113 failed: ${r.message}`);
    assert.equal(liveRows(), 0);
    assert.equal(sandboxRows(), REPAIRED.length, 'rows were relabelled, not deleted');
  });

  it('is idempotent — a second run changes nothing and does not fail', () => {
    freshCase('bz_0113_idempotent');
    assert.ok(run0113().ok);
    const after1 = sandboxRows();
    const second = run0113();
    assert.ok(second.ok, `re-running 0113 failed: ${second.message}`);
    assert.equal(sandboxRows(), after1);
    assert.equal(liveRows(), 0);
  });

  // Gate 1, observed refusing. On a LIVE platform some of these rows may be
  // genuine, and relabelling them would hide real money from every LIVE query.
  it('refuses when the platform is not declared SANDBOX', () => {
    freshCase('bz_0113_live_mode');
    psql(url, `UPDATE platform_settings SET value='LIVE' WHERE key='platform_mode' AND environment='GLOBAL'`);
    const r = run0113();
    assert.ok(!r.ok, 'a LIVE platform must refuse the repair');
    assert.match(r.message, /platform_mode is LIVE, not SANDBOX/);
    assert.equal(liveRows(), REPAIRED.length, 'a refused migration must change nothing');
  });

  it('refuses when platform_mode is missing entirely', () => {
    freshCase('bz_0113_no_mode');
    psql(url, `DELETE FROM platform_settings WHERE key='platform_mode'`);
    const r = run0113();
    assert.ok(!r.ok);
    assert.match(r.message, /platform_mode is <unset>/);
  });

  // Gate 2, observed refusing. platform_mode says what the platform is meant to
  // be; this asks whether the data agrees.
  it('refuses when a LIVE credential exists, because LIVE rows might be genuine', () => {
    freshCase('bz_0113_live_key');
    psql(url, `INSERT INTO api_keys (id, merchant_id, name, key_prefix, key_hash, environment)
               VALUES (gen_random_uuid(), '${M}', 'live key', 'bz_live_', 'hash', 'LIVE')`);
    const r = run0113();
    assert.ok(!r.ok, 'a LIVE api key must stop the blanket repair');
    assert.match(r.message, /found 1 LIVE api_keys/);
    assert.equal(liveRows(), REPAIRED.length);
  });

  it('refuses when a LIVE merchant application exists', () => {
    freshCase('bz_0113_live_app');
    psql(url, `INSERT INTO merchant_applications (id, desired_handle, business_name, email, environment)
               VALUES (gen_random_uuid(), 'live_merchant', 'Live Co', 'live@example.test', 'LIVE')`);
    const r = run0113();
    assert.ok(!r.ok);
    assert.match(r.message, /1 LIVE merchant_applications/);
  });

  // Atomicity. A refusal must not leave half the tables repaired: the gates run
  // before any UPDATE, and sqlx wraps the migration in a transaction, but the
  // property worth asserting is the observable one.
  it('a refusal leaves every table untouched', () => {
    freshCase('bz_0113_atomic');
    psql(url, `UPDATE platform_settings SET value='LIVE' WHERE key='platform_mode' AND environment='GLOBAL'`);
    run0113();
    for (const t of REPAIRED) {
      assert.equal(
        Number(psql(url, `SELECT count(*) FROM ${t} WHERE environment='SANDBOX'`).trim()), 0,
        `${t} was partially repaired by a refused migration`,
      );
    }
  });
});
