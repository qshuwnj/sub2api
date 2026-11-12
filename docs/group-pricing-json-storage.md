# Large group-pricing saves

## Boundary

Group pricing remains the same JSON API and Go types. Model names, tariff
intervals, billing rules, wire decimals, and group priority are unchanged.
Only `groups.model_pricing` changes from PostgreSQL JSONB to JSON. Channel
pricing tables and their JSONB columns are not changed.

The application reads the entire group document and does not search or index
inside this column. `jsonb_set` still expands the existing JSONB tariff tree
for a one-card edit. An opaque JSON-text replacement avoids this tree
allocation. Local process-memory measurements are diagnostic evidence, not a
guarantee of production peak memory or proof of the cause of a server SIGKILL.

## Save contract

- Unchanged pricing skips the large column entirely.
- Changed pricing is marshalled with the existing decimal-preserving encoder.
- An update checks group ID, the original `updated_at`, and `deleted_at`.
- Pricing, metadata, the new timestamp, and the scheduler outbox commit in
  one transaction. A stale snapshot returns the existing conflict error.
- Database or outbox failures roll back the transaction; they are not
  converted into successful saves or retried automatically.
- Existing caller-owned transactions and repositories without a SQL handle
  retain their existing full-writer behavior.

Migration `256_group_pricing_json_storage.sql` casts the existing document
without rebuilding its fields or updating business rows. It has a five-second
lock timeout and a sixty-second statement timeout. Historical migration files
are unchanged; replay tests restore the historical JSONB column inside their
own rolled-back transaction.

## Validation

Run from `backend`:

```sh
go test ./internal/repository ./migrations -count=1
go test -tags integration ./internal/repository \
  -run 'Test(GroupPricingPostgres|Migration239)' -count=1 -v
```

The repository integration harness normally needs Docker. The local native
overlay used for the repair is separate tooling, restricted to a loopback
scratch database, and is not shipped in production.

Tests cover large-document save/readback, unchanged tariffs, card additions
and removals, null versus empty arrays, wire decimals, concurrent stale
writers, deleted groups, failure-stage rollback, migration preservation,
historical migration replay, and the reverse schema cast.

For release validation, `SUB2API_TEST_PRICING_DOCUMENT` may point to a local
JSON request containing `model_pricing`. It is never checked into the
repository. Without it, a large deterministic synthetic document is used.

## Production gate and rollback

Do not automatically retry a failed production save or reuse a consumed
one-shot permit. Before rollout:

1. Reconfirm the deployed repository, commit, service, database, and current
   column type. Take a fresh backup and verify restoration separately.
2. Rehearse the migration against a copy of that backup, compare business
   data, and assess table-lock time and database headroom.
3. Obtain explicit approval for the schema migration and any new one-card
   save. Keep unrelated channels and billing configuration untouched.
4. Deploy the matching code and migration together, then require a live
   deployment, health checks, and exact tariff readback.

A binary-only rollback to a release using `jsonb_set(model_pricing,...)`
is not sufficient after migration 256. Stop writes first. In a backed-up
maintenance transaction, restore JSONB with:

```sql
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '60s';
ALTER TABLE groups
  ALTER COLUMN model_pricing TYPE JSONB USING model_pricing::jsonb;
```

That reverse cast can use more memory and lock the table; it must be rehearsed
and separately approved. The migration ledger must be coordinated with the
chosen binary and forward-repair plan. Do not casually delete migration
checksums or restore an old backup over newer customer data.
