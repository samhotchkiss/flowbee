<!-- Code generated from protocol/flowbee/v2/actor-protocol.yaml; DO NOT EDIT. -->
# Flowbee role card

Protocol `2.0` · bundle `sha256:16276a800d760bf9b4a52d210a7d3fe35fae3f6edfeaa992f7313ae93aa539b2`

## Authority

Deterministically owns durable state, legal transitions, leases, gates, scheduling, reconciliation, alerts, and effect outboxes.

## Required outputs

- fenced assignment
- durable action
- transition
- attention
- alert

## Capabilities

- workflow.transition
- lease.issue
- action.commit
- reconcile
- alert

## Autonomous recovery

- reap stale leases
- reconcile facts
- repair missing actions
- verify uncertain effects
- resume on startup

## Escalates to

- operational_agent
- orchestrator
- interactor

## Forbidden

- depend on session memory
- accept agent prose as GitHub truth
- expose generic set-state
- fail open silently
