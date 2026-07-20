<!-- Code generated from protocol/flowbee/v2/actor-protocol.yaml; DO NOT EDIT. -->
# `orchestrator_route_absent`

Protocol `2.0` · bundle `sha256:16276a800d760bf9b4a52d210a7d3fe35fae3f6edfeaa992f7313ae93aa539b2`

- Severity: `critical`
- Owner: `flowbee`
- Escalates to: `interactor`
- Automatic: `true`
- Maximum automatic attempts: `3`
- Named proof: `project_route_absent_alert`

## Invariant

each project has exactly one current Interactor-to-Orchestrator route

## Authoritative predicate

ready project work has no compatible paired Orchestrator route

## Repair

Run the domain action `reconcile_project_actor_route` under fence `project_id+registration_version`. Reuse the existing identity and idempotency key. Never mutate state directly or invent a replacement action.
