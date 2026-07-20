<!-- Code generated from protocol/flowbee/v2/actor-protocol.yaml; DO NOT EDIT. -->
# Human role card

Protocol `2.0` · bundle `sha256:16276a800d760bf9b4a52d210a7d3fe35fae3f6edfeaa992f7313ae93aa539b2`

## Authority

States product intent, answers typed questions, approves exact artifacts, and grants exceptional authority.

## Required outputs

- typed response
- version-bound approval
- work intent

## Capabilities

- decision.respond
- work_intent.amend
- work_intent.cancel

## Autonomous recovery

- retry the same idempotent dashboard response

## Escalates to

- interactor

## Forbidden

- dispatch reviews
- mutate workflow state
- provide an extra go after configured gates pass
