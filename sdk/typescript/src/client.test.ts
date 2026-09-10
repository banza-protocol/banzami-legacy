import { describe, it, expect, vi, beforeEach } from 'vitest';
import { BanzamiClient, isDeveloperPlatformKey, environmentFromKey, resolveEnvironment } from './client.js';
import { BanzamiApiError, BanzamiConfigError, BanzamiAuthError } from './errors.js';

// ---------------------------------------------------------------------------
// Helpers
//
// The gateway is JWT-authenticated: the client exchanges the API key at
// POST /v1/auth/token before any protected request. The fetch stub
// auto-answers that exchange and delegates the rest to the handler, so
// tests only describe the protected-request behaviour.
// ---------------------------------------------------------------------------

const AUTH = '/v1/auth/token';
const isAuth = (url: unknown): boolean => String(url).includes(AUTH);

function jsonResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
}
function tokenBody(expiresInMs = 3_600_000): unknown {
  return {
    token:       'jwt-test-token',
    expires_at:  new Date(Date.now() + expiresInMs).toISOString(),
    token_type:  'Bearer',
    environment: 'sandbox',
  };
}

/** Stub fetch: auto-answers the token exchange, delegates everything else. */
function stubFetch(handler: (url: string, init: RequestInit) => Response): void {
  vi.stubGlobal('fetch', vi.fn().mockImplementation((url: string, init: RequestInit = {}) =>
    Promise.resolve(isAuth(url) ? jsonResponse(200, tokenBody()) : handler(String(url), init)),
  ));
}
/** Token exchange + a single fixed response for the real request. */
function mockFetch(status: number, body: unknown): void {
  stubFetch(() => jsonResponse(status, body));
}

const allCalls    = () => (fetch as ReturnType<typeof vi.fn>).mock.calls as unknown[][];
const nonAuthCalls = () => allCalls().filter((c) => !isAuth(c[0]));
const authCalls    = () => allCalls().filter((c) =>  isAuth(c[0]));

/** The last real (non-auth) request. */
function lastFetchCall(): { url: string; init: RequestInit } {
  const c = nonAuthCalls();
  const [url, init] = c[c.length - 1];
  return { url: url as string, init: init as RequestInit };
}

let client: BanzamiClient;

beforeEach(() => {
  client = new BanzamiClient({ baseUrl: 'https://api.test.ao', apiKey: 'bz_live_testkey' });
});

// ---------------------------------------------------------------------------
// Environment / key model
// ---------------------------------------------------------------------------

describe('environmentFromKey', () => {
  it('classifies sandbox keys (legacy and _sk_ / _pk_)', () => {
    expect(environmentFromKey('bz_test_abc')).toBe('sandbox');
    expect(environmentFromKey('bz_test_sk_abc')).toBe('sandbox');
    expect(environmentFromKey('bz_test_pk_abc')).toBe('sandbox');
  });

  it('classifies live keys (legacy and _sk_ / _pk_)', () => {
    expect(environmentFromKey('bz_live_abc')).toBe('live');
    expect(environmentFromKey('bz_live_sk_abc')).toBe('live');
    expect(environmentFromKey('bz_live_pk_abc')).toBe('live');
  });

  it('returns null for unrecognised or empty keys', () => {
    expect(environmentFromKey('')).toBeNull();
    expect(environmentFromKey('sk_test_stripe')).toBeNull();
  });
});

describe('resolveEnvironment', () => {
  it('infers from the key prefix when no environment is given', () => {
    expect(resolveEnvironment('bz_test_sk_x')).toBe('sandbox');
    expect(resolveEnvironment('bz_live_sk_x')).toBe('live');
  });

  it('honours an explicit environment that matches the key', () => {
    expect(resolveEnvironment('bz_test_sk_x', 'sandbox')).toBe('sandbox');
    expect(resolveEnvironment('bz_live_sk_x', 'live')).toBe('live');
  });

  it('throws on live environment with a sandbox key', () => {
    expect(() => resolveEnvironment('bz_test_sk_x', 'live')).toThrow(BanzamiConfigError);
    expect(() => resolveEnvironment('bz_test_sk_x', 'live')).toThrow(/environment\/key mismatch/);
  });

  it('throws on sandbox environment with a live key', () => {
    expect(() => resolveEnvironment('bz_live_sk_x', 'sandbox')).toThrow(BanzamiConfigError);
  });

  it('falls back to the base URL when the key has no recognised prefix', () => {
    expect(resolveEnvironment('placeholder', undefined, 'https://sandbox-api.banzami.com')).toBe('sandbox');
  });

  it('defaults to live for an unrecognised key and no hints', () => {
    expect(resolveEnvironment('placeholder')).toBe('live');
  });
});

describe('client environment wiring', () => {
  it('exposes isSandbox inferred from a test key', () => {
    const c = new BanzamiClient({ apiKey: 'bz_test_sk_x' });
    expect(c.environment).toBe('sandbox');
    expect(c.isSandbox).toBe(true);
  });

  it('exposes isProduction inferred from a live key', () => {
    const c = new BanzamiClient({ apiKey: 'bz_live_sk_x' });
    expect(c.environment).toBe('live');
    expect(c.isProduction).toBe(true);
  });

  it('throws at construction on an environment/key mismatch', () => {
    expect(() => new BanzamiClient({ apiKey: 'bz_test_sk_x', environment: 'live' }))
      .toThrow(BanzamiConfigError);
  });

  it('picks the sandbox base URL by default for a sandbox key', async () => {
    const c = new BanzamiClient({ apiKey: 'bz_test_sk_x' });
    mockFetch(200, { id: '1' });
    await c.getTransaction('1');
    expect(lastFetchCall().url).toContain('https://sandbox-api.banzami.com');
  });
});

// ---------------------------------------------------------------------------
// Credential model: two kinds of key, authenticated two different ways
// ---------------------------------------------------------------------------

describe('credential model', () => {
  // A Developer Console key IS the bearer credential. Sending it to the
  // merchant exchange endpoint fails auth, which is what a developer following
  // the documented path hit on their very first call.
  it('uses a Developer Platform key directly, without exchanging it', async () => {
    const c = new BanzamiClient({ apiKey: 'bz_test_sk_c1' });
    mockFetch(200, { id: '1' });
    await c.getTransaction('1');
    expect(authCalls().length).toBe(0);
    const { init } = lastFetchCall();
    expect((init.headers as Record<string, string>)['Authorization']).toBe('Bearer bz_test_sk_c1');
  });

  it('still exchanges a merchant API key for a JWT', async () => {
    const c = new BanzamiClient({ baseUrl: 'https://api.test.ao', apiKey: 'bz_live_testkey' });
    mockFetch(200, { id: '1' });
    await c.getTransaction('1');
    expect(authCalls().length).toBeGreaterThan(0);
    const { init } = lastFetchCall();
    expect((init.headers as Record<string, string>)['Authorization']).toBe('Bearer jwt-test-token');
  });

  it('classifies both credential shapes', () => {
    for (const k of ['bz_test_sk_a', 'bz_live_sk_a', 'bz_test_pk_a', 'bz_live_pk_a']) {
      expect(isDeveloperPlatformKey(k), k).toBe(true);
    }
    // A merchant key carries hex straight after the environment prefix.
    for (const k of ['bz_test_hexkey', 'bz_live_testkey', '', 'nonsense']) {
      expect(isDeveloperPlatformKey(k), k).toBe(false);
    }
  });
});

// ---------------------------------------------------------------------------
// Authorization header
// ---------------------------------------------------------------------------

describe('authorization', () => {
  it('sends the exchanged JWT (not the raw API key) as Bearer on requests', async () => {
    mockFetch(200, { id: '1' });
    await client.getTransaction('1');
    const { init } = lastFetchCall();
    expect((init.headers as Record<string, string>)['Authorization']).toBe('Bearer jwt-test-token');
  });

  it('throws BanzamiApiError on 4xx', async () => {
    mockFetch(404, { code: 'NOT_FOUND', message: 'not found' });
    await expect(client.getTransaction('missing')).rejects.toBeInstanceOf(BanzamiApiError);
  });

  it('BanzamiApiError carries status and code', async () => {
    mockFetch(422, { code: 'INVALID_AMOUNT', message: 'amount must be positive' });
    const err = await client.getTransaction('x').catch((e) => e) as BanzamiApiError;
    expect(err.status).toBe(422);
    expect(err.code).toBe('INVALID_AMOUNT');
  });
});

// ---------------------------------------------------------------------------
// createTransaction
// ---------------------------------------------------------------------------

describe('createTransaction', () => {
  const tx = {
    id: 'tx-1', merchant_id: 'm-1', amount_minor: 5000, currency: 'AOA',
    status: 'PENDING', created_at: '', updated_at: '',
  };

  it('POSTs to /v1/transactions', async () => {
    mockFetch(201, tx);
    await client.createTransaction({ idempotencyKey: 'ik-1', amountMinor: 5000 });
    const { url, init } = lastFetchCall();
    expect(url).toBe('https://api.test.ao/v1/transactions');
    expect(init.method).toBe('POST');
  });

  it('sends idempotency_key and amount_minor in body', async () => {
    mockFetch(201, tx);
    await client.createTransaction({ idempotencyKey: 'ik-2', amountMinor: 1000 });
    const body = JSON.parse(lastFetchCall().init.body as string);
    expect(body.idempotency_key).toBe('ik-2');
    expect(body.amount_minor).toBe(1000);
  });

  it('defaults currency to AOA and transaction_type to payment', async () => {
    mockFetch(201, tx);
    await client.createTransaction({ idempotencyKey: 'ik-3', amountMinor: 500 });
    const body = JSON.parse(lastFetchCall().init.body as string);
    expect(body.currency).toBe('AOA');
    expect(body.transaction_type).toBe('payment');
  });

  it('passes optional fields through', async () => {
    mockFetch(201, tx);
    await client.createTransaction({
      idempotencyKey:   'ik-4',
      amountMinor:      2000,
      currency:         'USD',
      description:      'test payment',
      walletId:         'w-1',
      transactionType:  'top_up',
    });
    const body = JSON.parse(lastFetchCall().init.body as string);
    expect(body.currency).toBe('USD');
    expect(body.description).toBe('test payment');
    expect(body.wallet_id).toBe('w-1');
    expect(body.transaction_type).toBe('top_up');
  });

  it('puts no pricing selector on the wire at all', async () => {
    mockFetch(201, tx);
    await client.createTransaction({ idempotencyKey: 'ik-5', amountMinor: 5000 });
    const body = JSON.parse(lastFetchCall().init.body as string);

    // These three used to be accepted and forwarded, defended as "reference
    // only, never a fee". The defence was accurate about the wire and wrong
    // about the consequence: the caller chose the reference, and the reference
    // chose the price. The operator now resolves pricing from the merchant's
    // assigned profile, server-side.
    expect('business_category' in body).toBe(false);
    expect('pricing_profile' in body).toBe(false);
    expect('fee_policy_ref' in body).toBe(false);

    // And still no amount, which was true before and remains true.
    expect(body.fee_minor).toBeUndefined();
    expect(body.rate_bps).toBeUndefined();
    expect(body.operator_fee).toBeUndefined();
    expect(body.application_fee).toBeUndefined();
  });

  it('cannot be made to send a pricing selector by passing one anyway', async () => {
    mockFetch(201, tx);
    // A JavaScript caller has no types. Excess properties must not reach the
    // wire just because TypeScript would have refused them at compile time.
    await client.createTransaction({
      idempotencyKey: 'ik-6',
      amountMinor:    100,
      businessCategory: 'DONATION',
      pricingProfile:   'STANDARD',
      feePolicyRef:     'pol_donation_standard',
    } as Parameters<typeof client.createTransaction>[0]);
    const body = JSON.parse(lastFetchCall().init.body as string);
    expect('business_category' in body).toBe(false);
    expect('pricing_profile' in body).toBe(false);
    expect('fee_policy_ref' in body).toBe(false);
  });
});

// ---------------------------------------------------------------------------
// Payment links
// ---------------------------------------------------------------------------

describe('createPaymentLink', () => {
  const link = {
    id: 'pl-1', slug: 'abc123', merchant_id: 'm-1', wallet_id: 'w-1',
    currency: 'AOA', status: 'ACTIVE', created_at: '', updated_at: '',
  };

  it('POSTs to /v1/payment-links', async () => {
    mockFetch(201, link);
    await client.createPaymentLink({ merchantId: 'm-1', walletId: 'w-1' });
    const { url, init } = lastFetchCall();
    expect(url).toBe('https://api.test.ao/v1/payment-links');
    expect(init.method).toBe('POST');
  });

  it('sends merchant_id and wallet_id', async () => {
    mockFetch(201, link);
    await client.createPaymentLink({ merchantId: 'm-1', walletId: 'w-1' });
    const body = JSON.parse(lastFetchCall().init.body as string);
    expect(body.merchant_id).toBe('m-1');
    expect(body.wallet_id).toBe('w-1');
  });

  it('defaults currency to AOA', async () => {
    mockFetch(201, link);
    await client.createPaymentLink({ merchantId: 'm-1', walletId: 'w-1' });
    const body = JSON.parse(lastFetchCall().init.body as string);
    expect(body.currency).toBe('AOA');
  });

  it('serialises expiresAt as ISO string', async () => {
    mockFetch(201, link);
    const date = new Date('2026-12-31T00:00:00Z');
    await client.createPaymentLink({ merchantId: 'm-1', walletId: 'w-1', expiresAt: date });
    const body = JSON.parse(lastFetchCall().init.body as string);
    expect(body.expires_at).toBe('2026-12-31T00:00:00.000Z');
  });
});

describe('listPaymentLinks', () => {
  it('GETs /v1/payment-links with merchant_id query param', async () => {
    mockFetch(200, { data: [], next_cursor: undefined });
    await client.listPaymentLinks({ merchantId: 'm-1', limit: 10 });
    const { url } = lastFetchCall();
    expect(url).toContain('/v1/payment-links');
    expect(url).toContain('merchant_id=m-1');
    expect(url).toContain('limit=10');
  });
});

describe('getPaymentLink', () => {
  it('GETs /v1/payment-links/{id}', async () => {
    mockFetch(200, { id: 'pl-1' });
    await client.getPaymentLink('pl-1');
    expect(lastFetchCall().url).toBe('https://api.test.ao/v1/payment-links/pl-1');
  });
});

describe('cancelPaymentLink', () => {
  it('DELETEs /v1/payment-links/{id}', async () => {
    mockFetch(200, { id: 'pl-1', status: 'CANCELLED' });
    await client.cancelPaymentLink('pl-1');
    const { url, init } = lastFetchCall();
    expect(url).toBe('https://api.test.ao/v1/payment-links/pl-1');
    expect(init.method).toBe('DELETE');
  });
});

describe('getPublicPaymentLink', () => {
  it('GETs /v1/public/pay/{slug}', async () => {
    mockFetch(200, { id: 'pl-1', slug: 'abc123' });
    await client.getPublicPaymentLink('abc123');
    expect(lastFetchCall().url).toBe('https://api.test.ao/v1/public/pay/abc123');
  });
});

describe('getPaymentLinkStatus', () => {
  it('GETs /v1/public/pay/{slug}/status', async () => {
    mockFetch(200, { paid: true });
    const result = await client.getPaymentLinkStatus('abc123');
    expect(result.paid).toBe(true);
    expect(lastFetchCall().url).toBe('https://api.test.ao/v1/public/pay/abc123/status');
  });
});

// ---------------------------------------------------------------------------
// Payment QR (SDK owns the payload)
// ---------------------------------------------------------------------------

const LINK = {
  id: 'pl-1', slug: 'abc123', merchant_id: 'm-1', wallet_id: 'w-1',
  amount_minor: 150000, currency: 'AOA', description: '1 Kg de Arroz',
  status: 'ACTIVE' as const, created_at: 't', updated_at: 't',
};

describe('paymentLinkQr (pure)', () => {
  it('returns the official QR payload as the canonical pay URL — no network call', () => {
    vi.stubGlobal('fetch', vi.fn()); // must NOT be called
    const qr = client.paymentLinkQr(LINK);
    // live test client → default pay base
    expect(qr.qrValue).toBe('https://pay.banzami.com/pay/abc123');
    expect(qr.paymentUrl).toBe(qr.qrValue);
    expect(qr.slug).toBe('abc123');
    expect(qr.paymentLinkId).toBe('pl-1');
    expect(qr.amountMinor).toBe(150000);
    expect(qr.currency).toBe('AOA');
    expect(qr.description).toBe('1 Kg de Arroz');
    expect(qr.status).toBe('ACTIVE');
    expect((fetch as ReturnType<typeof vi.fn>)).not.toHaveBeenCalled();
  });

  it('enriches with caller-supplied recipient identity', () => {
    const qr = client.paymentLinkQr(LINK, { recipientHandle: '@fm65', recipientName: 'Fidel Monteiro' });
    expect(qr.recipientHandle).toBe('@fm65');
    expect(qr.recipientName).toBe('Fidel Monteiro');
  });

  it('defaults recipient identity + open amount to null', () => {
    const qr = client.paymentLinkQr({ ...LINK, amount_minor: undefined, description: undefined });
    expect(qr.recipientHandle).toBeNull();
    expect(qr.recipientName).toBeNull();
    expect(qr.amountMinor).toBeNull();
    expect(qr.description).toBeNull();
  });

  it('carries live environment metadata for a live client', () => {
    const qr = client.paymentLinkQr(LINK);
    expect(qr.environment).toBe('live');
    expect(qr.isSandbox).toBe(false);
  });

  it('carries sandbox metadata + default sandbox pay base for a sandbox client', () => {
    const sandbox = new BanzamiClient({ apiKey: 'bz_test_x' });
    const qr = sandbox.paymentLinkQr(LINK);
    expect(qr.environment).toBe('sandbox');
    expect(qr.isSandbox).toBe(true);
    expect(qr.qrValue).toBe('https://pay.banzami.com/pay/abc123');
  });

  it('honours an explicit payBaseUrl override (trailing slash trimmed)', () => {
    const custom = new BanzamiClient({ apiKey: 'bz_test_x', payBaseUrl: 'https://pay.sandbox.example/' });
    expect(custom.paymentLinkQr(LINK).qrValue).toBe('https://pay.sandbox.example/pay/abc123');
  });

  // Domain canonicalization guards: payment URLs MUST use pay.banzami.com and
  // the canonical /pay/<slug> path, and MUST NEVER use a legacy .org domain
  // (the BANZA protocol lives at banza.network).
  it('emits the canonical /pay/<slug> path', () => {
    const qr = client.paymentLinkQr(LINK);
    expect(qr.qrValue).toBe('https://pay.banzami.com/pay/abc123');
    expect(qr.qrValue).toContain('/pay/');
    expect(qr.paymentUrl).toBe(qr.qrValue);
  });

  it('never emits a legacy .org payment URL (live or sandbox)', () => {
    expect(client.paymentLinkQr(LINK).qrValue).not.toMatch(/\.org\b/);
    const sandbox = new BanzamiClient({ apiKey: 'bz_test_x' });
    expect(sandbox.paymentLinkQr(LINK).qrValue).not.toMatch(/\.org\b/);
  });

  it('defaults to the pay.banzami.com host in both environments', () => {
    expect(new BanzamiClient({ apiKey: 'bz_live_x' }).paymentLinkQr(LINK).qrValue)
      .toContain('https://pay.banzami.com/');
    expect(new BanzamiClient({ apiKey: 'bz_test_x' }).paymentLinkQr(LINK).qrValue)
      .toContain('https://pay.banzami.com/');
  });
});

describe('getPaymentLinkQr (fetch + derive)', () => {
  it('fetches the link over JWT auth and returns the official payload', async () => {
    mockFetch(200, LINK);
    const qr = await client.getPaymentLinkQr('pl-1', { recipientHandle: '@fm65' });
    // the only protected call is the authenticated payment-link fetch
    const { url, init } = lastFetchCall();
    expect(url).toBe('https://api.test.ao/v1/payment-links/pl-1');
    const headers = new Headers(init.headers);
    expect(headers.get('authorization')).toBe('Bearer jwt-test-token');
    // the raw API key never travels on the protected endpoint
    expect(JSON.stringify(init.headers ?? {})).not.toContain('bz_live_testkey');
    expect(String(url)).not.toContain('bz_live_testkey');
    // derived payload
    expect(qr.qrValue).toBe('https://pay.banzami.com/pay/abc123');
    expect(qr.recipientHandle).toBe('@fm65');
    expect(authCalls().length).toBe(1); // a single key→JWT exchange happened
  });
});

// ---------------------------------------------------------------------------
// Retry logic
// ---------------------------------------------------------------------------

describe('retry', () => {
  const tx = { id: 'tx-1', merchant_id: 'm-1', amount_minor: 5000, currency: 'AOA', status: 'PENDING', created_at: '', updated_at: '' };

  it('retries on 503 and succeeds on third attempt', async () => {
    let n = 0;
    stubFetch(() => {
      n++;
      return n <= 2 ? jsonResponse(503, { code: 'OVERLOAD', message: 'overload' }) : jsonResponse(200, tx);
    });
    const retryClient = new BanzamiClient({ baseUrl: 'https://api.test.ao', apiKey: 'bz_live_testkey', maxRetries: 3, retryDelay: 0 });
    const result = await retryClient.createTransaction({ idempotencyKey: 'ik-retry', amountMinor: 5000 });
    expect(result.id).toBe('tx-1');
    expect(n).toBe(3); // protected request attempts (auth exchange excluded)
  });

  it('does not retry on 422', async () => {
    let n = 0;
    stubFetch(() => { n++; return jsonResponse(422, { code: 'INVALID_AMOUNT', message: 'bad amount' }); });
    const retryClient = new BanzamiClient({ baseUrl: 'https://api.test.ao', apiKey: 'bz_live_testkey', maxRetries: 3, retryDelay: 0 });
    await expect(retryClient.createTransaction({ idempotencyKey: 'ik-no-retry', amountMinor: -1 })).rejects.toBeInstanceOf(BanzamiApiError);
    expect(n).toBe(1);
  });

  it('uses the same idempotency key on all retries', async () => {
    const tx2 = { ...tx, id: 'tx-2', amount_minor: 1000 };
    const capturedKeys: string[] = [];
    let n = 0;
    stubFetch((_url, init) => {
      n++;
      const headers = init.headers as Record<string, string>;
      if (headers['Idempotency-Key']) capturedKeys.push(headers['Idempotency-Key']);
      return n <= 2 ? jsonResponse(503, { code: 'OVERLOAD', message: 'overload' }) : jsonResponse(200, tx2);
    });
    const retryClient = new BanzamiClient({ baseUrl: 'https://api.test.ao', apiKey: 'bz_live_testkey', maxRetries: 3, retryDelay: 0 });
    await retryClient.createTransaction({ idempotencyKey: 'ik-idempotent', amountMinor: 1000 });
    expect(capturedKeys.length).toBe(3);
    expect(capturedKeys.every(k => k === capturedKeys[0])).toBe(true);
  });
});

// ---------------------------------------------------------------------------
// Auth token exchange (API key → JWT)
// ---------------------------------------------------------------------------

describe('auth token exchange', () => {
  it('exchanges the API key at /v1/auth/token before the first request', async () => {
    mockFetch(200, { id: '1' });
    await client.getTransaction('1');
    const ac = authCalls();
    expect(ac.length).toBe(1);
    expect(ac[0][0]).toBe('https://api.test.ao/v1/auth/token');
    const body = JSON.parse((ac[0][1] as RequestInit).body as string);
    expect(body.api_key).toBe('bz_live_testkey');
  });

  it('never sends the raw API key to a protected endpoint', async () => {
    mockFetch(200, { id: '1' });
    await client.getTransaction('1');
    // the only place the raw key appears is the auth body — never a Bearer header
    for (const [url, init] of nonAuthCalls()) {
      const auth = ((init as RequestInit).headers as Record<string, string>)['Authorization'];
      expect(auth).toBe('Bearer jwt-test-token');
      expect(auth).not.toContain('bz_live_testkey');
      expect(String(url)).not.toContain('bz_live_testkey');
    }
  });

  it('caches the JWT and reuses it across requests', async () => {
    mockFetch(200, { id: '1' });
    await client.getTransaction('1');
    await client.getTransaction('2');
    await client.getTransaction('3');
    expect(authCalls().length).toBe(1);     // exchanged once
    expect(nonAuthCalls().length).toBe(3);  // three protected requests
  });

  it('re-exchanges when the cached token has expired', async () => {
    // token that is already past the refresh skew
    vi.stubGlobal('fetch', vi.fn().mockImplementation((url: string) =>
      Promise.resolve(isAuth(url)
        ? jsonResponse(200, { token: 'jwt-test-token', expires_at: new Date(Date.now() - 1000).toISOString(), token_type: 'Bearer', environment: 'sandbox' })
        : jsonResponse(200, { id: '1' })),
    ));
    await client.getTransaction('1');
    await client.getTransaction('2');
    expect(authCalls().length).toBe(2);     // re-exchanged because expired
  });

  it('re-exchanges once and retries after a 401 INVALID_TOKEN', async () => {
    let protectedHits = 0;
    stubFetch(() => {
      protectedHits++;
      return protectedHits === 1
        ? jsonResponse(401, { code: 'INVALID_TOKEN', message: 'token is invalid or expired' })
        : jsonResponse(200, { id: 'ok' });
    });
    const r = await client.getTransaction('1') as { id: string };
    expect(r.id).toBe('ok');
    expect(protectedHits).toBe(2);        // first 401, retried after re-auth
    expect(authCalls().length).toBe(2);   // initial + re-exchange
  });

  it('throws BanzamiAuthError when the API key is rejected at exchange', async () => {
    vi.stubGlobal('fetch', vi.fn().mockImplementation((url: string) =>
      Promise.resolve(isAuth(url)
        ? jsonResponse(401, { code: 'UNAUTHORIZED', message: 'invalid API key' })
        : jsonResponse(200, { id: '1' })),
    ));
    await expect(client.getTransaction('1')).rejects.toBeInstanceOf(BanzamiAuthError);
  });

  it('throws BanzamiAuthError when the auth endpoint is unreachable', async () => {
    vi.stubGlobal('fetch', vi.fn().mockImplementation((url: string) =>
      isAuth(url) ? Promise.reject(new Error('network down')) : Promise.resolve(jsonResponse(200, { id: '1' })),
    ));
    await expect(client.getTransaction('1')).rejects.toBeInstanceOf(BanzamiAuthError);
  });
});

// ---------------------------------------------------------------------------
// Wallet Accounts (ADR-042) + app-defined settlement (ADR-029)
// ---------------------------------------------------------------------------

describe('wallet accounts', () => {
  const wa = {
    id: 'wa-1', wallet_id: 'w-1', purpose: 'CAMPAIGN', reference_type: 'DOA_CAMPAIGN',
    reference_id: 'camp-1', label: 'Campanha', status: 'ACTIVE',
    available_balance_minor: 0, currency: 'AOA', created_at: '2026-06-30T00:00:00Z',
  };

  it('createWalletAccount posts to /wallet-accounts with the right body', async () => {
    mockFetch(201, wa);
    await client.createWalletAccount({
      walletId: 'w-1', purpose: 'CAMPAIGN',
      referenceType: 'DOA_CAMPAIGN', referenceId: 'camp-1', label: 'Campanha',
    });
    const { url, init } = lastFetchCall();
    expect(url).toContain('/wallet-accounts');
    expect(init.method).toBe('POST');
    const body = JSON.parse(init.body as string);
    expect(body.wallet_id).toBe('w-1');
    expect(body.purpose).toBe('CAMPAIGN');
    expect(body.reference_type).toBe('DOA_CAMPAIGN');
    expect(body.reference_id).toBe('camp-1');
  });

  it('listWalletAccounts GETs with wallet_id query', async () => {
    mockFetch(200, { data: [wa] });
    await client.listWalletAccounts('w-1');
    const { url, init } = lastFetchCall();
    expect(url).toContain('/wallet-accounts?wallet_id=w-1');
    expect((init.method ?? 'GET')).toBe('GET');
  });

  it('getWalletAccount GETs by id', async () => {
    mockFetch(200, wa);
    await client.getWalletAccount('wa-1');
    expect(lastFetchCall().url).toContain('/wallet-accounts/wa-1');
  });
});

describe('app-defined application settlement', () => {
  const settled = {
    id: 'set-1', owner_ref: 'camp-1', status: 'CREATED',
    gross_amount_minor: 200000, application_fee_minor: 10000, net_amount_minor: 190000,
    currency: 'AOA', environment: 'sandbox', created_at: '2026-06-30T00:00:00Z',
    completed_at: null, failure_reason: null,
  };

  it('createBusinessApplicationSettlement sends @names and NO amount', async () => {
    mockFetch(201, settled);
    await client.createBusinessApplicationSettlement({
      idempotencyKey: 'doa-camp-1-settle',
      sourceAccountId: 'wa-1',
      beneficiaryBanzaName: '@maria',
      feeDestinationBanzaName: '@doa',
      referenceType: 'DOA_CAMPAIGN',
      referenceId: 'camp-1',
    });
    const { url, init } = lastFetchCall();
    expect(url).toContain('/application-settlements');
    expect(init.method).toBe('POST');
    const body = JSON.parse(init.body as string);
    expect(body.source_account_id).toBe('wa-1');
    expect(body.beneficiary_banza_name).toBe('@maria');
    expect(body.fee_destination_banza_name).toBe('@doa');
    expect(body.reason).toBe('CAMPAIGN_CLOSE');
    // the app never sends an amount or a computed fee
    expect(body.gross_amount_minor).toBeUndefined();
    expect(body.amount_minor).toBeUndefined();
    expect(body.application_fee_minor).toBeUndefined();
  });

  it('the SDK cannot be made to send a fee rate', async () => {
    // A JavaScript caller has no types, so removing the field from the interface
    // is not by itself a control. The rate was the one thing on this request a
    // caller could set that changed what it paid: a non-zero application_fee_bps
    // made the operator skip pricing entirely.
    mockFetch(201, settled);
    await client.createBusinessApplicationSettlement({
      idempotencyKey: 'doa-camp-2-settle',
      sourceAccountId: 'wa-1',
      beneficiaryBanzaName: '@maria',
      feeDestinationBanzaName: '@doa',
      applicationFeeBps: 4999,
    } as never);
    const body = JSON.parse(lastFetchCall().init.body as string);
    expect(body.application_fee_bps).toBeUndefined();
  });

  it('createApplicationSettlement cannot be made to send a pricing selector', async () => {
    mockFetch(201, settled);
    // A JavaScript caller has no types. These three used to be accepted here
    // and forwarded, and each one is a rule-matching dimension in the
    // operator's pricing engine — so sending them was choosing a tariff.
    await client.createApplicationSettlement({
      idempotencyKey:      'as-selector',
      ownerRef:            'campaign_42',
      sourceWalletId:      'w-1',
      beneficiaryWalletId: 'w-2',
      businessCategory:    'DONATION',
      pricingProfile:      'sandbox-reference',
      feePolicyRef:        'pol_donation_standard',
    } as Parameters<typeof client.createApplicationSettlement>[0]);
    const body = JSON.parse(lastFetchCall().init.body as string);
    expect('business_category' in body).toBe(false);
    expect('pricing_profile' in body).toBe(false);
    expect('fee_policy_ref' in body).toBe(false);
  });
});

// ---------------------------------------------------------------------------
// Payment Sessions (ADR-043) — bound to a wallet account
// ---------------------------------------------------------------------------

describe('payment sessions', () => {
  const session = {
    session_id: 'ps-1', wallet_account_id: 'wa-1', currency: 'AOA', amount_minor: 5000,
    purpose: 'DONATION', reference_type: 'DOA_DONATION', reference_id: 'intent-1',
    status: 'ACTIVE', expires_at: null, created_at: '2026-06-30T00:00:00Z',
    interfaces: [
      { type: 'PAYMENT_LINK', value: 'https://pay/abc', format: 'URL' },
      { type: 'DEEP_LINK', value: 'banzami://pay/abc', format: 'URL' },
      { type: 'DYNAMIC_QR', value: 'banzami://pay/abc', format: 'QR_PAYLOAD', qr_url: '/qr' },
    ],
  };

  it('createPaymentSession posts wallet_account_id + amount to /payment-sessions', async () => {
    mockFetch(201, session);
    await client.createPaymentSession({
      walletAccountId: 'wa-1', amountMinor: 5000,
      purpose: 'DONATION', referenceType: 'DOA_DONATION', referenceId: 'intent-1',
      description: 'Doa',
    });
    const { url, init } = lastFetchCall();
    expect(url).toContain('/payment-sessions');
    expect(init.method).toBe('POST');
    const body = JSON.parse(init.body as string);
    expect(body.wallet_account_id).toBe('wa-1');
    expect(body.amount_minor).toBe(5000);
    expect(body.reference_id).toBe('intent-1');
    expect(body.currency).toBe('AOA');
  });

  it('createPaymentSession omits amount as null for an open-amount session', async () => {
    mockFetch(201, { ...session, amount_minor: null });
    await client.createPaymentSession({ walletAccountId: 'wa-1' });
    const body = JSON.parse(lastFetchCall().init.body as string);
    expect(body.amount_minor).toBeNull();
  });

  it('getPaymentSession GETs by id and exposes the canonical interfaces array', async () => {
    mockFetch(200, session);
    const s = await client.getPaymentSession('ps-1');
    expect(lastFetchCall().url).toContain('/payment-sessions/ps-1');
    const qr = client.paymentSessionInterface(s, 'DYNAMIC_QR');
    expect(qr?.value).toBe('banzami://pay/abc');
    expect(client.paymentSessionInterface(s, 'PAYMENT_LINK')?.value).toBe('https://pay/abc');
  });

  it('listPaymentSessions GETs with status filter', async () => {
    mockFetch(200, { data: [session] });
    await client.listPaymentSessions({ status: 'PAID', limit: 10 });
    const { url } = lastFetchCall();
    expect(url).toContain('/payment-sessions?');
    expect(url).toContain('status=PAID');
    expect(url).toContain('limit=10');
  });
});

describe('getBusinessMe', () => {
  it('GETs /v1/integration and returns the typed self profile', async () => {
    mockFetch(200, {
      environment: 'SANDBOX', id: 'm-1', handle: 'doa', business_name: 'Doa',
      business_account_type: 'MERCHANT', status: 'ACTIVE', kyb_status: 'APPROVED',
      verified: true, category: 'Doações e causas', pricing_category: 'DONATION',
      subcategory: null, wallet_ready: true, settlement_ready: true,
      pricing: { category: 'DONATION', profile: '', rule_key: 'donation-standard', fee_bps: 50, found: true },
      wallet: { ready: true, wallet_id: 'w-1', currency: 'AOA', status: 'ACTIVE', primary_account_id: 'pa-1', application_account_id: '' },
      settlement: { ready: true, enabled: true, blockers: [] },
      blockers: [],
    });
    const c = new BanzamiClient({ apiKey: 'bz_test_sk_x' });
    const me = await c.getBusinessMe();
    expect(lastFetchCall().url).toContain('/v1/integration');
    expect(me.handle).toBe('doa');
    expect(me.kyb_status).toBe('APPROVED');
    expect(me.settlement_ready).toBe(true);
    expect(me.pricing_category).toBe('DONATION');
    expect(me.wallet.primary_account_id).toBe('pa-1');
    expect(me.settlement.blockers).toEqual([]);
  });
});
