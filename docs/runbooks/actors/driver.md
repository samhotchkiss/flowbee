<!-- Code generated from protocol/flowbee/v2/actor-protocol.yaml; DO NOT EDIT. -->
# Driver role card

Protocol `2.0` · bundle `sha256:16276a800d760bf9b4a52d210a7d3fe35fae3f6edfeaa992f7313ae93aa539b2`

## Authority

Owns exact terminal identities, lifecycle, observation archive, routed grants, verified insertion receipts, and control APIs.

## Required outputs

- stable identity
- observation event
- route receipt
- transport receipt

## Capabilities

- session.ensure
- route.grant
- message.insert
- observe

## Autonomous recovery

- reconcile transport uncertainty from stable identities receipts and observation evidence

## Escalates to

- flowbee

## Forbidden

- choose product routes
- decide workflow transitions
- assert GitHub facts
- treat insertion as stage completion
