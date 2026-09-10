# The one simulated assertion, classified

Version: 1.0

`tests/phase0/developer-platform-e2e.sh` reports `pass=20 fail=0 simulated=1
blocked=0`. A green aggregate with a simulation inside it is worth exactly as
much as the classification of that simulation, so here it is.

## What it is

`F0-DP-011 — webhook event emitted/simulated (signed payload)`

The harness records:

> event emission pipeline reachable; Banza-Signature HMAC + retry/backoff
> 1m/5m/30m/2h/8h max-5 + idempotency verified in code+unit tests; live outbound
> 2xx delivery needs a public HTTPS sink — excluded by no-external/no-public

So within that harness the exclusion is deliberate and coherent: it is a suite
with a no-external-egress policy, and it says so rather than pretending.

## The classification

The question is not whether the harness is honest about its own scope. It is
whether **live signed webhook delivery has deployed proof anywhere**.

When this was first written the answer was no. The Sandbox ran a webhook sink
whose log held one line — `{"msg":"webhook sink listening","port":8090}` — and
zero deliveries, and the suites that would have proved it end to end both
required a developer project that could not yet be created.

**Verdict at the time: (B) — missing deployed proof.**

## Closed

The stated ground for the simulation was that a live outbound 2xx delivery needs
a public HTTPS sink, which the no-external policy excluded. `sandbox-webhook.banzami.com`
is now that sink: a genuinely public HTTPS receiver, reachable exactly because
the SSRF policy (RA-023) refuses private targets and no exception was added for
it. The ground no longer holds, so the assertion is made for real.

`tests/phase0/developer-platform-e2e.sh` reports **pass=24 fail=0 simulated=0
blocked=0** on the deployed Sandbox. F0-DP-011 now:

- registers an endpoint pointing at the public sink,
- has a payer confirm a payment link,
- waits for the deployed runtime to deliver on its own schedule,
- verifies `Banza-Signature` with an implementation written from the published
  contract that imports nothing from `services/api-gateway/internal/webhook`,
  judged as of the moment each delivery arrived,
- and re-checks the same delivery under a wrong secret, which must be rejected —
  a verifier that accepted everything would report a green delivery for a
  platform that signed nothing.

`tools/e2e/webhooks/cap-webhook-001-sandbox-e2e.mjs` carries the wider capability
at 55/55 with 0 blocked, including the retry schedule observed on the published
backoff carrying the same event id.

There is no simulated assertion left to classify. This file stays because the
reason it existed — that a `simulated=1` nobody classifies is indistinguishable
from a gap nobody noticed — is worth keeping written down.
