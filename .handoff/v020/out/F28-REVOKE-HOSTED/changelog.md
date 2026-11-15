### Changed

- `kudurru_root_anchor_updater_acquisitions_total` has a new `canceled` outcome for a root query that was cut off because the writer term or the process ended. `refused` now counts only queries after which the updater stopped for its term (a refused reply or a store error). The metric's `/metrics` help text lists all five outcomes.
