# AGENTS.md — Core Rules

## Non-Negotiable Security Rules
- Phone numbers are NEVER stored raw. Always hash via internal/crypto.
- PII masking format: rawPhone[:4] + "****" + rawPhone[len(rawPhone)-2:]. MUST include length guard (len > 8) before slicing.
- API keys must use crypto/rand, never math/rand.

## Database & State Rules (Postgres/Redis)
- All Postgres updates affecting balances MUST check RowsAffected == 0 to prevent race conditions.
- Redis caching is for BAD ACTORS ONLY (score ≤ 20). Never cache clean users (score 85).
- Webhook handlers MUST return 200 OK immediately. All DB/Redis work must happen in goroutines.
- Goroutine workers processing Redis keyspace events must implement idempotency/deduplication to prevent double-processing.

## Architecture
- cmd/ → entry points
- internal/handlers/ → HTTP only, no business logic
- internal/service/ → all business logic lives here
- internal/domain/ → structs only, no logic
## SQL Safety
- The column name allowlist in incrementMetric must never 
  be removed. Never interpolate column names from user input.

## Deployment & CLI Operations
- Database migrations (cmd/migrate/main.go) must be executed as a one-off ECS task prior to updating the live web server or background worker services.
- The hard-purge CLI (cmd/purge/main.go) must be run on a daily scheduled cadence using an ECS Scheduled Task (EventBridge cron rule).

## Code Sync Points
- **Scoring Formula**: The inline score calculation in the Meta CAPI goroutine (`internal/billing/ingestion.go`), weekly audience sync query (`cmd/audience_sync/main.go`), and `redis_trust.go` (`internal/service/redis_trust.go`) must always remain in sync. If the scoring logic changes, all three locations must be updated simultaneously. Use comment `// SYNC WITH: internal/service/redis_trust.go EvaluateRisk` to find these locations.