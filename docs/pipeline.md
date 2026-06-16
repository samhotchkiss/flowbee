# Flowbee Pipeline Stages

The Flowbee pipeline moves an issue through the following stages, in order:

1. **spec_authoring** — A spec author drafts the specification describing what should be built.
2. **spec_review** — The drafted spec is reviewed and refined until it is approved.
3. **ready** — The approved spec is queued and waiting for an engineering worker to pick it up.
4. **building** — An engineering worker implements the change in an isolated working directory.
5. **review_pending** — The completed change is queued and waiting for code review.
6. **code_review** — A reviewer examines the diff for correctness, quality, and adherence to the spec.
7. **mergeable** — The change has passed review and is cleared to merge.
8. **merging** — The change is being merged into the target branch.
9. **done** — The change is merged and the pipeline run is complete.
