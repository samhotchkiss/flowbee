# Flowbee Pipeline Stages

The Flowbee pipeline moves an issue through the following stages, in order.

1. **spec_authoring** — A spec is drafted that captures what the work should accomplish.
2. **spec_review** — The drafted spec is reviewed and refined before any code is written.
3. **ready** — The approved spec is queued and waiting for a worker to pick it up.
4. **building** — A worker implements the change in an isolated working directory.
5. **review_pending** — The completed change awaits assignment to code review.
6. **code_review** — The diff is reviewed for correctness, quality, and adherence to the spec.
7. **mergeable** — The change has passed review and is cleared to merge.
8. **merging** — The change is being merged into the target branch.
9. **done** — The change is merged and the pipeline run is complete.
