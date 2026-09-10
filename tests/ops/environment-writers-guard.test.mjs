/**
 * Every writer of an environment-scoped financial row must name the environment.
 *
 * `environment` is not a label. It is a filter: the public proof lookup, the
 * wallet payment list, KYC case selection and webhook dispatch all read it. And
 * the column carries `DEFAULT 'LIVE'`, which is a fail-OPEN default — a writer
 * that omits the column does not fail, it silently asserts the row is real money.
 *
 * Nine writers omitted it. Every Sandbox transfer, payment link, QR code, payout
 * and webhook endpoint therefore claimed to be LIVE, and the Sandbox proof
 * lookup could not see records it had just created. That is BZM-F993-38E2's
 * second cause, and it produced 272 mislabelled rows.
 *
 * This is the grep that found them, kept. It is deliberately syntactic: it does
 * not know whether the bound value is correct, only that the writer had to think
 * about it. The value itself is proven by the runtime tests beside each writer.
 */
import { execFileSync } from 'node:child_process';
import assert from 'node:assert/strict';
import { describe, it } from 'node:test';
import { join } from 'node:path';
import { readFileSync, readdirSync } from 'node:fs';

const REPO = join(import.meta.dirname, '../..');

/**
 * Tables that carry an environment column, read from the migrations rather than
 * hardcoded — a table that gains the column later must gain the guard with it.
 *
 * Both ways it can arrive. The first draft read only `ALTER TABLE … ADD COLUMN
 * environment`, which is how migration 0018 added it to seven tables, and
 * therefore protected seven. Eleven more declare it inline in their CREATE
 * TABLE, and those were unguarded — including transaction_proofs, whose
 * environment decides whether a public receipt resolves at all.
 */
function environmentScopedTables() {
  const dir = join(REPO, 'db/migrations');
  const tables = new Set();
  for (const f of readdirSync(dir).filter((f) => f.endsWith('.sql'))) {
    const sql = readFileSync(join(dir, f), 'utf8');
    for (const m of sql.matchAll(/ALTER\s+TABLE\s+(?:IF\s+EXISTS\s+)?(\w+)\s+ADD\s+COLUMN\s+(?:IF\s+NOT\s+EXISTS\s+)?environment\b/gi)) {
      tables.add(m[1].toLowerCase());
    }
    // CREATE TABLE x ( … environment … ). The body is taken to the matching
    // close paren so a later table's column cannot be attributed to this one.
    for (const m of sql.matchAll(/CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?(\w+)\s*\(/gi)) {
      const open = m.index + m[0].length - 1;
      let depth = 0, end = open;
      for (let i = open; i < sql.length; i++) {
        if (sql[i] === '(') depth++;
        else if (sql[i] === ')') { depth--; if (depth === 0) { end = i; break; } }
      }
      if (/^\s*environment\s/im.test(sql.slice(open + 1, end))) tables.add(m[1].toLowerCase());
    }
  }
  // platform_settings.environment is a SCOPE, not a universe: it holds 'GLOBAL'
  // and says which stacks a setting applies to. Its writers rely on that default
  // deliberately, and it is the one place where relying on the default is right.
  tables.delete('platform_settings');
  return tables;
}

/** Statements that insert into one of those tables, across Go and Rust sources. */
function insertSites(tables) {
  const out = [];
  for (const t of tables) {
    let lines = [];
    try {
      // NOT \\b: git grep -E is POSIX ERE, where \\b is a backspace character, not a
      // word boundary. Written that way the pattern matches nothing and the guard
      // passes on every input — which is exactly what the first mutation showed.
      lines = execFileSync('git', ['grep', '-n', '-i', '-E', `INSERT INTO ${t}([^a-zA-Z0-9_]|$)`, '--',
        'core/**/*.rs', 'services/**/*.go'], { cwd: REPO, encoding: 'utf8' })
        .trim().split('\n').filter(Boolean);
    } catch (e) {
      if (e.status !== 1) throw e;
    }
    for (const line of lines) out.push({ table: t, line });
  }
  return out;
}

const isTestFile = (line) =>
  /_test\.go:|_tests\.rs:|\/tests\/|\/test\//.test(line);

/**
 * Read the statement's column list. INSERT statements here are multi-line, so
 * take the source from the matched line up to the first closing paren that ends
 * the column list.
 */
function columnListAt(file, lineNo) {
  const src = readFileSync(join(REPO, file), 'utf8').split('\n');
  const window = src.slice(lineNo - 1, lineNo + 8).join('\n');
  const open = window.indexOf('(');
  if (open === -1) return '';
  let depth = 0;
  for (let i = open; i < window.length; i++) {
    if (window[i] === '(') depth++;
    else if (window[i] === ')') {
      depth--;
      if (depth === 0) return window.slice(open + 1, i);
    }
  }
  return window;
}

describe('environment-scoped writers', () => {
  const tables = environmentScopedTables();

  it('the migration set still declares the tables this guard protects', () => {
    // A rename or a squashed migration would otherwise turn the whole guard into
    // a no-op that passes loudly.
    assert.ok(tables.size >= 18, `expected every environment-scoped table, found ${tables.size}: ${[...tables].sort()}`);
    for (const t of [
      // added by 0018
      'transfers', 'transactions', 'payouts', 'payment_links', 'qr_codes', 'webhook_endpoints', 'api_keys',
      // declared inline, and unguarded until this test learned to read a CREATE TABLE
      'transaction_proofs', 'wallet_payments', 'app_settlements', 'merchant_applications',
      'kyc_cases', 'kyc_evidence', 'merchant_kyb_documents', 'operator_fees',
      'pricing_rules', 'pricing_profiles', 'fee_policies',
    ]) {
      assert.ok(tables.has(t), `${t} lost its environment column, or this guard lost sight of it`);
    }
  });

  it('no production INSERT omits the environment column', () => {
    const offenders = [];
    for (const { table, line } of insertSites(tables)) {
      if (isTestFile(line)) continue;
      const [file, lineNo] = [line.slice(0, line.indexOf(':')), Number(line.split(':')[1])];
      const cols = columnListAt(file, lineNo);
      if (!/\benvironment\b/.test(cols)) {
        offenders.push(`${file}:${lineNo} (INSERT INTO ${table})`);
      }
    }
    assert.deepEqual(
      offenders, [],
      'a financial row is being written without naming its environment — the column ' +
      'defaults to LIVE, so this silently records the row as real money:\n  ' +
      offenders.join('\n  '),
    );
  });

  it('no writer hardcodes a literal environment', () => {
    // 'SANDBOX' written into the SQL text would make the Sandbox correct and the
    // deployment irrelevant — the environment must come from the process, so that
    // one binary cannot be right in one universe and wrong in the other.
    const offenders = [];
    for (const { table, line } of insertSites(tables)) {
      if (isTestFile(line)) continue;
      const [file, lineNo] = [line.slice(0, line.indexOf(':')), Number(line.split(':')[1])];
      const src = readFileSync(join(REPO, file), 'utf8').split('\n')
        .slice(lineNo - 1, lineNo + 14).join('\n');
      if (/VALUES[\s\S]{0,400}?'(LIVE|SANDBOX)'/i.test(src)) {
        offenders.push(`${file}:${lineNo} (INSERT INTO ${table})`);
      }
    }
    assert.deepEqual(offenders, [], 'an environment literal is baked into a writer');
  });
});
