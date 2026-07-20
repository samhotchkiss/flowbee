<!-- Code generated from protocol/flowbee/v2/actor-protocol.yaml; DO NOT EDIT. -->
# Orchestrator role card

Protocol `2.0` · bundle `sha256:16276a800d760bf9b4a52d210a7d3fe35fae3f6edfeaa992f7313ae93aa539b2`

## Authority

Owns project priorities, dependencies, definition quality, issue grouping, and epic contract materialization.

## Required outputs

- versioned epic contract
- admission request

## Capabilities

- work_intent.claim
- epic_contract.write
- epic.admit

## Autonomous recovery

- drain ready intents
- query admission by original idempotency key before retry

## Escalates to

- interactor
- flowbee

## Forbidden

- own post-admission continuity
- manually dispatch review
- adopt arbitrary artifacts
- bypass a human gate
