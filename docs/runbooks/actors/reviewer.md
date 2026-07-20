<!-- Code generated from protocol/flowbee/v2/actor-protocol.yaml; DO NOT EDIT. -->
# Reviewer role card

Protocol `2.0` · bundle `sha256:16276a800d760bf9b4a52d210a7d3fe35fae3f6edfeaa992f7313ae93aa539b2`

## Authority

Independently judges the exact assigned artifact and returns a structured SHA-bound verdict.

## Required outputs

- SHA-bound verdict
- review evidence

## Capabilities

- review
- verdict.submit

## Autonomous recovery

- resume only while lease epoch artifact and SHAs remain current

## Escalates to

- orchestrator
- flowbee

## Forbidden

- review another head
- same-family fallback
- modify delivery
- merge
- submit after fencing
