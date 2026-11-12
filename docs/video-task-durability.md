# Accepted video task durability

## Storage boundary

`video_task_states` (migration 257) keeps the accepted provider task ID, user,
API key, original group/account binding and immutable creation-time billing
parameters in PostgreSQL. It contains no provider credentials or signed media
URLs. Redis eviction or OOM no longer removes the ownership of new video jobs.

Owner and billing snapshot are one database write. Conflicting account, group
or snapshot data is rejected. Settlement is monotonic: a settled task cannot
be changed back to unsettled. The production usage-billing transaction and its
active/archive dedup tables still own exactly-once charging.

Legacy tasks may be backfilled only from their exact user/API-key/group Redis
binding plus original billing snapshot. Database errors do not fall back to
Redis, and missing state does not authorize another account lookup.

## HTTP behavior

- A known storage outage before create returns 503 and sends no generation.
- A storage outage during an ownership query returns 503
  `video_state_unavailable` with `Retry-After`, not a false 404.
- A genuinely absent owner still returns 404; no cross-account discovery.
- If AtlasCloud/Dapacai has already accepted create but persistence fails, the
  response stays 202, retains the original task ID/provider body and includes
  `persistence_status=unavailable`, `retry_generation=false`.
- A post-acceptance write has its own bounded context; cancelling the client
  request does not cancel saving the recovery identity.

Clients must retain the accepted ID and query that job. A warning or query
failure is not permission to submit another paid generation.

## Verification and rollout

Service tests cover Redis loss, exact ownership, legacy backfill and database
failure. SQL tests cover immutable snapshots and monotonic settlement. The
opt-in `VIDEO_BILLING_TEST_DSN` test uses a disposable PostgreSQL database and
concurrent real SQL billing calls.

This is a local patch, not proof of a production release. Deploy migration 257
with the related application patch only after scoped validation. Do not bundle
the unrelated pricing migration 256 or other pending pricing edits by accident.
The two incident videos were separately restored from existing provider media;
that restoration intentionally preserved refunded orders and did not call the
normal recharge-on-recovery endpoints.
