<!-- Code generated from protocol/flowbee/v2/actor-protocol.yaml; DO NOT EDIT. -->
# `orchestrator_claim_expired`

Protocol `2.0` · bundle `sha256:16276a800d760bf9b4a52d210a7d3fe35fae3f6edfeaa992f7313ae93aa539b2`

- Severity: `critical`
- Owner: `flowbee`
- Escalates to: `interactor`
- Automatic: `true`
- Maximum automatic attempts: `3`
- Named proof: `orchestrator_claim_restart`

## Invariant

ready intent is claimed by exactly its paired Orchestrator

## Authoritative predicate

orchestrator route lease expired without an epic contract

## Repair

Run the domain action `fence_and_requeue_orchestrator_claim` under fence `route_action_id+lease_epoch+agent_run_id`. Reuse the existing identity and idempotency key. Never mutate state directly or invent a replacement action.
