-- Release B: the default that answered for nine writers is removed.
--
-- `environment` carried DEFAULT 'LIVE' on eighteen tables. That is a fail-OPEN
-- default on a column that is a filter: a writer that forgets it does not fail,
-- it silently records the row as real money. Nine writers forgot, and 272 rows
-- on this platform claimed to be LIVE until 0113 repaired them.
--
-- The two-phase rule is why this is a separate migration in a separate release.
-- Removing a default in the same release that stops depending on it leaves no
-- window in which to notice a writer that was missed: everything would break at
-- once, at deploy, with no way to tell a real omission from a rollout order
-- problem. Release A shipped the writers and proved them — every row created on
-- this platform after that deploy says SANDBOX. This closes the door behind them.
--
-- After this, a writer that omits the column violates NOT NULL and fails loudly
-- at the moment of the write, which is the whole point: the column says which
-- financial universe a record belongs to, and there is no safe value to guess.
--
-- The DATA is untouched. Nothing is inserted, updated or deleted; every existing
-- row keeps the environment it has. Dropping a default changes only what happens
-- to the NEXT write that stays silent.

-- ---------------------------------------------------------------------------
-- Gate — the same platform authority 0113 used.
--
-- On a LIVE platform this is still the right change, but it is not a change to
-- make without someone watching: a writer that has never been exercised there
-- would begin failing on its first call. Refusing here keeps that a deliberate
-- act rather than a side effect of a rollout.
-- ---------------------------------------------------------------------------
DO $$
DECLARE mode TEXT;
BEGIN
    SELECT value INTO mode
      FROM platform_settings
     WHERE key = 'platform_mode' AND environment = 'GLOBAL';

    IF mode IS DISTINCT FROM 'SANDBOX' THEN
        RAISE EXCEPTION
            'refusing to remove the environment default: platform_mode is %, not '
            'SANDBOX. On a LIVE platform an unexercised writer would begin failing '
            'on its first call; do this with someone watching.', COALESCE(mode, '<unset>');
    END IF;
END $$;

-- ---------------------------------------------------------------------------
-- Precondition — nothing may still be claiming LIVE.
--
-- If a row appeared after 0113 with environment='LIVE', a writer was missed and
-- removing the default would turn a silent mislabel into a hard failure in
-- production rather than in a test. Better to find out here.
-- ---------------------------------------------------------------------------
DO $$
DECLARE live_rows INT;
BEGIN
    SELECT (SELECT count(*) FROM payment_links     WHERE environment = 'LIVE')
         + (SELECT count(*) FROM qr_codes          WHERE environment = 'LIVE')
         + (SELECT count(*) FROM transfers         WHERE environment = 'LIVE')
         + (SELECT count(*) FROM payouts           WHERE environment = 'LIVE')
         + (SELECT count(*) FROM webhook_endpoints WHERE environment = 'LIVE')
         + (SELECT count(*) FROM transactions      WHERE environment = 'LIVE')
         + (SELECT count(*) FROM wallet_payments   WHERE environment = 'LIVE')
         + (SELECT count(*) FROM transaction_proofs WHERE environment = 'LIVE')
      INTO live_rows;

    IF live_rows <> 0 THEN
        RAISE EXCEPTION
            'refusing to remove the environment default: % row(s) still claim LIVE '
            'after the 0113 repair, so a writer was missed.', live_rows;
    END IF;
END $$;

-- ---------------------------------------------------------------------------
-- The removal.
--
-- Named one by one rather than looped over information_schema, for the reason
-- 0113 gives: a schema change should say out loud which tables it alters, and a
-- table that gains the column later must be considered rather than swept in.
--
-- platform_settings is deliberately absent. Its `environment` is a SCOPE — it
-- holds 'GLOBAL' and says which stacks a setting applies to — and its writers
-- rely on that default on purpose. It is the one place where relying on the
-- default is correct.
-- ---------------------------------------------------------------------------
ALTER TABLE api_keys                ALTER COLUMN environment DROP DEFAULT;
ALTER TABLE app_settlements         ALTER COLUMN environment DROP DEFAULT;
ALTER TABLE fee_policies            ALTER COLUMN environment DROP DEFAULT;
ALTER TABLE kyc_cases               ALTER COLUMN environment DROP DEFAULT;
ALTER TABLE kyc_evidence            ALTER COLUMN environment DROP DEFAULT;
ALTER TABLE merchant_applications   ALTER COLUMN environment DROP DEFAULT;
ALTER TABLE merchant_kyb_documents  ALTER COLUMN environment DROP DEFAULT;
ALTER TABLE operator_fees           ALTER COLUMN environment DROP DEFAULT;
ALTER TABLE payment_links           ALTER COLUMN environment DROP DEFAULT;
ALTER TABLE payouts                 ALTER COLUMN environment DROP DEFAULT;
ALTER TABLE pricing_profiles        ALTER COLUMN environment DROP DEFAULT;
ALTER TABLE pricing_rules           ALTER COLUMN environment DROP DEFAULT;
ALTER TABLE qr_codes                ALTER COLUMN environment DROP DEFAULT;
ALTER TABLE transaction_proofs      ALTER COLUMN environment DROP DEFAULT;
ALTER TABLE transactions            ALTER COLUMN environment DROP DEFAULT;
ALTER TABLE transfers               ALTER COLUMN environment DROP DEFAULT;
ALTER TABLE wallet_payments         ALTER COLUMN environment DROP DEFAULT;
ALTER TABLE webhook_endpoints       ALTER COLUMN environment DROP DEFAULT;

-- ---------------------------------------------------------------------------
-- Postcondition — no financial table may still default its universe.
--
-- Asserted from the catalogue rather than from the list above, so a table that
-- acquires the column and a default between now and the next release is caught
-- by this migration's own successor rather than by a mislabelled row.
-- ---------------------------------------------------------------------------
DO $$
DECLARE remaining TEXT;
BEGIN
    SELECT string_agg(table_name, ', ' ORDER BY table_name) INTO remaining
      FROM information_schema.columns
     WHERE table_schema = 'public'
       AND column_name = 'environment'
       AND column_default IS NOT NULL
       AND table_name <> 'platform_settings';

    IF remaining IS NOT NULL THEN
        RAISE EXCEPTION 'environment still defaults on: %', remaining;
    END IF;
END $$;
