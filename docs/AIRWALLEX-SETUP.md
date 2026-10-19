# Airwallex sandbox access — status

**Blocker, owner action required (not code).** This is the only step in this
project that needs a human with hackathon-registered credentials; everything
else is built and runnable in mock mode without it.

## What's needed

Starter Kit 5 (Platform Spend Controller) lists setup as **"Request platform
access"** — this is an Airwallex sandbox client ID/secret issued per
hackathon participant, not a self-serve API key. Steps:

1. Open the hackathon's Airwallex Builder Guide (linked from the Overview tab
   on the HackerEarth challenge page) and follow the "Request platform
   access" link for Starter Kit 5 / Multi-Tenant Platforms.
2. This typically requires confirming the HackerEarth registration email and
   may take the organizers a day or two to approve (see forum thread: other
   participants ask "after how many days should I get approved for native
   APIs?" with no firm SLA given as of Oct 19).
3. Once approved, you'll receive:
   - `AIRWALLEX_CLIENT_ID`
   - `AIRWALLEX_API_KEY`
   - a sandbox base URL (normally `https://api-demo.airwallex.com`)
4. Drop them into `.env` (copy `.env.example`) and set `AIRWALLEX_MODE=live`.
   No code changes needed — `services/card-issuance/internal/airwallex` implements
   the same interface for both mock and live clients.

## Why this doesn't block the build

`services/card-issuance/internal/airwallex/mock.go` mirrors the real Issuing API
(`POST /issuing/cards/create`), Global Accounts (`GET
/global_accounts/{id}/transactions`) and webhook payload shapes closely
enough that the agent loop, ledger core, and TUI all run a full end-to-end
demo today against mock data. When live credentials land, flip
`AIRWALLEX_MODE=live` and re-run `make run-demo` — same code path.

## Open questions posted to the organizer forum (unanswered as of 2026-10-19)

From `blucca`'s thread (ticket 49355679334): project IP precedence between
the event FAQ and the general HackerEarth ToS, cash award currency/multi-award
eligibility, and treatment of AI-assisted implementation. Worth checking before
submission — answers weren't posted yet when this was written.
