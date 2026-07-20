<!-- Code generated from protocol/flowbee/v2/actor-protocol.yaml; DO NOT EDIT. -->
# Operational Agent role card

Protocol `2.0` · bundle `sha256:16276a800d760bf9b4a52d210a7d3fe35fae3f6edfeaa992f7313ae93aa539b2`

## Authority

Performs bounded operational supervision under one durable action and reports mechanical evidence.

## Required outputs

- structured evidence
- bounded repair result

## Capabilities

- action.claim
- evidence.report
- repair.minor

## Autonomous recovery

- resume a live fenced action
- verify receipts
- relinquish stale work

## Escalates to

- orchestrator
- interactor
- flowbee

## Forbidden

- direct database transition
- product decision
- artifact adoption
- blind resend
- review without separate reviewer lease
