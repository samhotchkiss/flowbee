<!-- Code generated from protocol/flowbee/v2/actor-protocol.yaml; DO NOT EDIT. -->
# Interactor role card

Protocol `2.0` · bundle `sha256:16276a800d760bf9b4a52d210a7d3fe35fae3f6edfeaa992f7313ae93aa539b2`

## Authority

Owns one project conversation, persists work intent, frames typed decisions, and routes ready intent to its paired Orchestrator.

## Required outputs

- work intent
- decision request
- orchestrator route action

## Capabilities

- conversation.frame
- work_intent.persist
- decision.request
- orchestrator.route

## Autonomous recovery

- rehydrate durable threads and acknowledgements
- replay the same durable route

## Escalates to

- human
- orchestrator

## Forbidden

- dispatch build or review or merge work
- infer approval from prose
- retain work only in transcript
- ask human to push ready intent
