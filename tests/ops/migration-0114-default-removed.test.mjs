/**
 * Migration 0114 turns a fail-open default into a fail-closed error.
 *
 * `environment` carried DEFAULT 'LIVE' on eighteen tables, so a writer that
 * forgot the column did not fail — it recorded the row as real money. Nine
 * writers forgot. After this, the same omission violates NOT NULL and fails at
 * the moment of the write, which is the only honest answer: the column says
 * which financial universe a record belongs to, and there is no safe guess.
 *
 * The gates matter more than the ALTERs. A default removed on a platform whose
 * writers have not been proved is a deploy that breaks everywhere at once, with
 * no way to tell a real omission from a rollout-order problem.
 *
 *   DATABASE_URL=postgres://localhost:5432/postgres node --test tests/ops/migration-0114-default-removed.test.mjs
 */
import { execFileSync } from 'node:child_process';
import { join } from 'node:path';
import assert from 'node:assert/strict';
import { after, before, describe, it } from 'node:test';

const REPO = join(import.meta.dirname, '../..');
const ADMIN = process.env.DATABASE_URL ?? 'postgres://localhost:5432/postgres';
const MIG = join(REPO, 'db/migrations');
const FILE = join(MIG, '0114_environment_default_removed.sql');

let tmplName;
let url;
const created = [];

const psql = (target, sql) =>
  execFileSync('psql', [target, '-v', 'ON_ERROR_STOP=1', '-Atc', sql],
    { encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] });

function run0114() {
  try {
    execFileSync('psql', [url, '-v', 'ON_ERROR_STOP=1', '-f', FILE],
      { encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] });
    return { ok: true, message: '' };
  } catch (e) {
    return { ok: false, message: `${e.stderr ?? ''}${e.stdout ?? ''}` };
  }
}

const stillDefaulting = () => Number(psql(url,
  `SELECT count(*) FROM information_schema.columns
    WHERE table_schema='public' AND column_name='environment'
      AND column_default IS NOT NULL AND table_name <> 'platform_settings'`).trim());

function freshCase(name) {
  url = ADMIN.replace(/\/[^/?]+(\?.*)?$/, `/${name}$1`);
  psql(ADMIN, `DROP DATABASE IF EXISTS ${name}`);
  psql(ADMIN, `CREATE DATABASE ${name} TEMPLATE ${tmplName}`);
  created.push(name);
}

describe('migration 0114 — the environment default is removed', () => {
  before(() => {
    tmplName = `bz_0114_tmpl_${process.pid}`;
    const tmplUrl = ADMIN.replace(/\/[^/?]+(\?.*)?$/, `/${tmplName}$1`);
    psql(ADMIN, `DROP DATABASE IF EXISTS ${tmplName}`);
    psql(ADMIN, `CREATE DATABASE ${tmplName}`);
    // Chain to 0113 — the repair must already have happened; 0114 is the subject.
    execFileSync('sqlx', ['migrate', 'run', '--source', MIG, '--target-version', '113', '-D', tmplUrl],
      { cwd: REPO, stdio: ['ignore', 'pipe', 'inherit'] });
  });

  after(() => {
    for (const d of created) { try { psql(ADMIN, `DROP DATABASE IF EXISTS ${d}`); } catch { /* best effort */ } }
    try { psql(ADMIN, `DROP DATABASE IF EXISTS ${tmplName}`); } catch { /* best effort */ } });

  it('leaves no financial table defaulting its universe', () => {
    freshCase('bz_0114_removed');
    assert.ok(stillDefaulting() >= 18, 'precondition: the defaults are there to remove');
    const r = run0114();
    assert.ok(r.ok, `0114 failed: ${r.message}`);
    assert.equal(stillDefaulting(), 0);
  });

  // The scope column. platform_settings.environment holds 'GLOBAL' and says
  // which stacks a setting applies to; its writers rely on that default on
  // purpose, and it is the one place where relying on a default is correct.
  it('does not touch the scope column that is meant to default', () => {
    freshCase('bz_0114_scope');
    assert.ok(run0114().ok);
    assert.equal(
      psql(url, `SELECT column_default FROM information_schema.columns
                  WHERE table_name='platform_settings' AND column_name='environment'`).trim(),
      "'GLOBAL'::text",
    );
  });

  // The point of the whole release, stated as a behaviour: a writer that stays
  // silent about the universe now fails instead of claiming real money.
  it('a writer that omits the column now fails instead of claiming LIVE', () => {
    freshCase('bz_0114_omission');
    psql(url, `INSERT INTO merchants (id, name, email) VALUES ('11111111-1111-4111-8111-111111111111','Seed','s@example.test');
               INSERT INTO ledger_accounts (id, account_type, name, currency)
                 VALUES ('44444444-4444-4444-8444-444444444441','LIABILITY','a','AOA'),
                        ('44444444-4444-4444-8444-444444444442','LIABILITY','r','AOA');
               INSERT INTO wallets (id, merchant_id, currency, available_account_id, reserved_account_id)
                 VALUES ('22222222-2222-4222-8222-222222222222','11111111-1111-4111-8111-111111111111','AOA',
                         '44444444-4444-4444-8444-444444444441','44444444-4444-4444-8444-444444444442')`);

    const omit = () => psql(url,
      `INSERT INTO payment_links (id, slug, merchant_id, wallet_id, currency)
       VALUES (gen_random_uuid(), 'omit-${Date.now()}', '11111111-1111-4111-8111-111111111111',
               '22222222-2222-4222-8222-222222222222', 'AOA')`);

    // Before: the omission succeeds and the row claims to be real money.
    omit();
    assert.equal(psql(url, `SELECT environment FROM payment_links LIMIT 1`).trim(), 'LIVE',
      'precondition: an omitted column used to mean LIVE');
    psql(url, 'DELETE FROM payment_links');

    assert.ok(run0114().ok);
    assert.throws(omit, /null value in column "environment"/,
      'after 0114 the same omission must fail at the write');
  });

  it('refuses when the platform is not declared SANDBOX', () => {
    freshCase('bz_0114_live_mode');
    psql(url, `UPDATE platform_settings SET value='LIVE' WHERE key='platform_mode' AND environment='GLOBAL'`);
    const r = run0114();
    assert.ok(!r.ok);
    assert.match(r.message, /platform_mode is LIVE, not SANDBOX/);
    assert.ok(stillDefaulting() >= 18, 'a refused migration must change nothing');
  });

  // If a row appeared after the repair still claiming LIVE, a writer was missed,
  // and removing the default would turn a silent mislabel into a production
  // failure rather than a test one.
  it('refuses when a row still claims LIVE, because that means a writer was missed', () => {
    freshCase('bz_0114_live_row');
    psql(url, `INSERT INTO merchants (id, name, email) VALUES ('11111111-1111-4111-8111-111111111111','Seed','s@example.test');
               INSERT INTO ledger_accounts (id, account_type, name, currency)
                 VALUES ('44444444-4444-4444-8444-444444444441','LIABILITY','a','AOA'),
                        ('44444444-4444-4444-8444-444444444442','LIABILITY','r','AOA');
               INSERT INTO wallets (id, merchant_id, currency, available_account_id, reserved_account_id)
                 VALUES ('22222222-2222-4222-8222-222222222222','11111111-1111-4111-8111-111111111111','AOA',
                         '44444444-4444-4444-8444-444444444441','44444444-4444-4444-8444-444444444442');
               INSERT INTO payment_links (id, slug, merchant_id, wallet_id, currency, environment)
                 VALUES (gen_random_uuid(), 'missed', '11111111-1111-4111-8111-111111111111',
                         '22222222-2222-4222-8222-222222222222', 'AOA', 'LIVE')`);
    const r = run0114();
    assert.ok(!r.ok);
    assert.match(r.message, /1 row\(s\) still claim LIVE/);
    assert.ok(stillDefaulting() >= 18);
  });
});
