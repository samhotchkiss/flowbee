<!-- Code generated from protocol/flowbee/v2/actor-protocol.yaml; DO NOT EDIT. -->
# `capacity_live_unavailable`

Protocol `2.0` · bundle `sha256:16276a800d760bf9b4a52d210a7d3fe35fae3f6edfeaa992f7313ae93aa539b2`

- Severity: `critical`
- Owner: `flowbee`
- Escalates to: `operational_agent`
- Automatic: `true`
- Maximum automatic attempts: `5`
- Named proof: `cache_never_routes`

## Invariant

cache and stale observations are visible but never routable

## Authoritative predicate

a required seat has no fresh verified live observation

## Repair

Run the domain action `retry_live_capacity_collection` under fence `seat_id+lineage_version+collector_epoch`. Reuse the existing identity and idempotency key. Never mutate state directly or invent a replacement action.
