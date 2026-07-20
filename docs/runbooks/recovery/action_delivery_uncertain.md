<!-- Code generated from protocol/flowbee/v2/actor-protocol.yaml; DO NOT EDIT. -->
# `action_delivery_uncertain`

Protocol `2.0` · bundle `sha256:16276a800d760bf9b4a52d210a7d3fe35fae3f6edfeaa992f7313ae93aa539b2`

- Severity: `critical`
- Owner: `flowbee`
- Escalates to: `operational_agent`
- Automatic: `true`
- Maximum automatic attempts: `3`
- Named proof: `driver_crash_uncertain_no_resend`

## Invariant

uncertain effects are verified before retry

## Authoritative predicate

action is verifying without conclusive receipt or mechanical evidence

## Repair

Run the domain action `verify_existing_action` under fence `action_id+action_epoch+payload_sha256+target_incarnation`. Reuse the existing identity and idempotency key. Never mutate state directly or invent a replacement action.
