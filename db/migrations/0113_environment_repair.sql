-- Rows that claimed to be real money.
--
-- `environment` defaults to 'LIVE'. Nine writers omitted the column, so on this
-- deployment 272 rows across six tables assert they are LIVE. They are not:
-- this platform has never been anything but Sandbox. The consequence is not
-- cosmetic — `environment` is a filter. The public proof lookup, the wallet
-- payment list and the webhook dispatch query all select on it, so a Sandbox
-- record labelled LIVE is invisible to the surfaces that should find it and
-- visible to the ones that should not. That is the second cause of the receipt
-- that verified as "does not exist or may have been forged".
--
-- The writers were fixed in code first (commit d4ccadda). This repairs the rows
-- they already wrote. It does NOT remove the column default: dropping a default
-- in the same release that stops depending on it leaves no window in which to
-- notice a writer that was missed. That is Release B's work.
--
-- Idempotent: a second run matches zero rows. Atomic: sqlx wraps a migration in
-- a transaction, so either every table is repaired or none is. Fail-closed: the
-- gates below refuse rather than guess.

-- ---------------------------------------------------------------------------
-- Gate 1 — the platform's own declaration.
--
-- platform_settings/platform_mode at GLOBAL scope is the environment authority
-- for migrations. If this platform is LIVE, some of those rows may be real and
-- this migration must not run: relabelling a genuine LIVE payment as Sandbox
-- would hide real money from every LIVE query, which is the same defect with
-- the sign reversed.
-- ---------------------------------------------------------------------------
DO $$
DECLARE mode TEXT;
BEGIN
    SELECT value INTO mode
      FROM platform_settings
     WHERE key = 'platform_mode' AND environment = 'GLOBAL';

    IF mode IS DISTINCT FROM 'SANDBOX' THEN
        RAISE EXCEPTION
            'refusing environment repair: platform_mode is %, not SANDBOX. '
            'On a LIVE platform some of these rows may be genuine and relabelling '
            'them would hide real money.', COALESCE(mode, '<unset>');
    END IF;
END $$;

-- ---------------------------------------------------------------------------
-- Gate 2 — corroboration, because one setting is one setting.
--
-- platform_mode says what the platform is configured to be. This asks whether
-- the data agrees: LIVE rows can only have been produced by a LIVE credential
-- or a LIVE merchant. If either exists, the blanket conclusion "every LIVE row
-- here is mislabelled" is no longer safe and a human must look.
--
-- On this deployment all 29 api_keys, all 29 merchant_applications and all 29
-- activation tokens are SANDBOX, and every other environment-scoped table is
-- uniformly SANDBOX. The six tables below are exactly the six whose writers
-- omitted the column.
-- ---------------------------------------------------------------------------
DO $$
DECLARE live_keys INT; live_apps INT;
BEGIN
    SELECT count(*) INTO live_keys FROM api_keys           WHERE environment = 'LIVE';
    SELECT count(*) INTO live_apps FROM merchant_applications WHERE environment = 'LIVE';

    IF live_keys > 0 OR live_apps > 0 THEN
        RAISE EXCEPTION
            'refusing environment repair: found % LIVE api_keys and % LIVE '
            'merchant_applications. Genuine LIVE activity may exist, so these rows '
            'cannot be repaired as a class.', live_keys, live_apps;
    END IF;
END $$;

-- ---------------------------------------------------------------------------
-- The repair.
--
-- One statement per table rather than a loop over information_schema: a data
-- migration should say out loud which tables it rewrites, and a table that
-- gains an environment column later must be considered deliberately rather than
-- swept in by a pattern match.
-- ---------------------------------------------------------------------------
UPDATE payment_links      SET environment = 'SANDBOX' WHERE environment = 'LIVE';
UPDATE qr_codes           SET environment = 'SANDBOX' WHERE environment = 'LIVE';
UPDATE transfers          SET environment = 'SANDBOX' WHERE environment = 'LIVE';
UPDATE payouts            SET environment = 'SANDBOX' WHERE environment = 'LIVE';
UPDATE webhook_endpoints  SET environment = 'SANDBOX' WHERE environment = 'LIVE';
UPDATE transactions       SET environment = 'SANDBOX' WHERE environment = 'LIVE';

-- ---------------------------------------------------------------------------
-- Postcondition.
--
-- Asserted rather than counted. A fixed expected count (272) would be right on
-- exactly one database and would fail on a fresh one, on a re-run, and on any
-- deployment that wrote one more row between the census and the migration. What
-- must hold everywhere is that nothing is left claiming LIVE.
-- ---------------------------------------------------------------------------
DO $$
DECLARE remaining INT;
BEGIN
    SELECT (SELECT count(*) FROM payment_links     WHERE environment = 'LIVE')
         + (SELECT count(*) FROM qr_codes          WHERE environment = 'LIVE')
         + (SELECT count(*) FROM transfers         WHERE environment = 'LIVE')
         + (SELECT count(*) FROM payouts           WHERE environment = 'LIVE')
         + (SELECT count(*) FROM webhook_endpoints WHERE environment = 'LIVE')
         + (SELECT count(*) FROM transactions      WHERE environment = 'LIVE')
      INTO remaining;

    IF remaining <> 0 THEN
        RAISE EXCEPTION 'environment repair incomplete: % rows still claim LIVE', remaining;
    END IF;
END $$;
