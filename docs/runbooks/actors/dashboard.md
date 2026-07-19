<!-- Code generated from protocol/flowbee/v2/actor-protocol.yaml; DO NOT EDIT. -->
# Dashboard role card

Protocol `2.0` · bundle `sha256:16276a800d760bf9b4a52d210a7d3fe35fae3f6edfeaa992f7313ae93aa539b2`

## Authority

Renders durable read models and submits authenticated version-bound human commands.

## Required outputs

- idempotent command
- durable message
- typed decision response

## Capabilities

- read.project
- read.portfolio
- decision.submit
- conversation.send

## Autonomous recovery

- refetch after cursor gaps
- retry the same request key

## Escalates to

- human

## Forbidden

- compute workflow truth locally
- convert prose to authorization
- report unacknowledged work complete
