# Historical calibration accepted; qualification authorized

Robert Herbaugh accepted the exact 138 proposed bounds on November 15, 2026:
"Approve these limits and run qualification."

The reviewed artifact is commit `071cdbcb4c1fe1b29de66346d171d9bd5b846970`,
including REVIEW.md, every raw historical vector and the old/new bound table.
The accepted calibration manifest SHA256 is:

```text
3b38bf0389bb04e2ed9b0546d79e276ffdbf70090ae8588afa7c5fb2a0bf7266
```

`approval.json` retains the actual owner decision and binds this digest. The
original pending template and review snapshot remain as recorded. Approval
permits one full 60-command interleaved paired qualification on the same
sandbox, guest identity, toolchain and hardware profile. Fixed product bounds,
20% relative threshold, noise floors, workloads and samples remain unchanged.
The 35 looser, 63 tighter and 40 unchanged hardware bounds derive solely from
immutable historical recordings. No candidate values were used.

Performance remains release-blocking until the final qualification passes.
This is not release, merge, deployment or GitHub-review acceptance.
