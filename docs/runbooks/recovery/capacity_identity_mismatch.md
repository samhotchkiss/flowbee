<!-- Code generated from protocol/flowbee/v2/actor-protocol.yaml; DO NOT EDIT. -->
# `capacity_identity_mismatch`

Protocol `2.0` · bundle `sha256:16276a800d760bf9b4a52d210a7d3fe35fae3f6edfeaa992f7313ae93aa539b2`

- Severity: `critical`
- Owner: `flowbee`
- Escalates to: `human`
- Automatic: `false`
- Maximum automatic attempts: `0`
- Named proof: `identity_swap_fences_seat`

## Invariant

observed provider identity and credential lineage match registration

## Authoritative predicate

expected and observed account or lineage differ

## Repair

Run the domain action `hold_seat_for_explicit_rebind` under fence `seat_id+expected_lineage_version`. Reuse the existing identity and idempotency key. Never mutate state directly or invent a replacement action.
