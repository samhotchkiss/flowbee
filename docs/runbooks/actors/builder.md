<!-- Code generated from protocol/flowbee/v2/actor-protocol.yaml; DO NOT EDIT. -->
# Builder role card

Protocol `2.0` · bundle `sha256:16276a800d760bf9b4a52d210a7d3fe35fae3f6edfeaa992f7313ae93aa539b2`

## Authority

Implements one bounded epic on its registered branch and worktree and reports structured evidence.

## Required outputs

- artifact
- progress fact
- build result

## Capabilities

- build
- test
- artifact.report

## Autonomous recovery

- relaunch the same affinity under a new incarnation
- resume rework after capacity reacquisition

## Escalates to

- orchestrator
- flowbee

## Forbidden

- choose pipeline state
- merge
- self-review
- touch another epic workspace
- call GitHub with worker credentials
