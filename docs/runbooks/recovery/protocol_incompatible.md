<!-- Code generated from protocol/flowbee/v2/actor-protocol.yaml; DO NOT EDIT. -->
# `protocol_incompatible`

Protocol `2.0` · bundle `sha256:16276a800d760bf9b4a52d210a7d3fe35fae3f6edfeaa992f7313ae93aa539b2`

- Severity: `critical`
- Owner: `flowbee`
- Escalates to: `operational_agent`
- Automatic: `false`
- Maximum automatic attempts: `0`
- Named proof: `incompatible_actor_gets_no_lease`

## Invariant

incompatible actors receive no new lease

## Authoritative predicate

protocol major schema hash or role bundle is unsupported

## Repair

Run the domain action `hold_actor_until_compatible` under fence `actor_run_id+bundle_hash+schema_hash`. Reuse the existing identity and idempotency key. Never mutate state directly or invent a replacement action.
