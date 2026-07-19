<!-- Code generated from protocol/flowbee/v2/actor-protocol.yaml; DO NOT EDIT. -->
# Capacity Collector role card

Protocol `2.0` · bundle `sha256:16276a800d760bf9b4a52d210a7d3fe35fae3f6edfeaa992f7313ae93aa539b2`

## Authority

Performs read-only on-host provider probes and reports sanitized identity-bound capacity observations.

## Required outputs

- signed usage observation
- identity lineage evidence

## Capabilities

- capacity.observe

## Autonomous recovery

- honor durable backoff
- retry live collection
- preserve last-good history

## Escalates to

- flowbee

## Forbidden

- choose routes
- export credentials
- promote cache data to routable
- relabel windows
- infer reset
