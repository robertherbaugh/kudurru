export const meta = {
  name: 'v020-post-verify',
  description: 'Independently verify fix commits that were integrated without a verification pass',
  phases: [{ title: 'Verify', detail: 'one adversarial verifier per package, read-only' }],
}

const SHARED = '/home/vercel-sandbox/kudurru/.context/v020-final-20261110'
const SCHEMA = {
  type: 'object',
  properties: {
    verdict: { type: 'string', enum: ['sound', 'problems'] },
    summary: { type: 'string' },
    resolved: { type: 'array', items: { type: 'string' } },
    problems: {
      type: 'array',
      items: {
        type: 'object',
        properties: {
          severity: { type: 'string', enum: ['blocker', 'major', 'minor', 'nit'] },
          file: { type: 'string' },
          issue: { type: 'string' },
          evidence: { type: 'string' },
          suggestedFix: { type: 'string' },
        },
        required: ['severity', 'file', 'issue', 'evidence', 'suggestedFix'],
      },
    },
    testsRun: { type: 'array', items: { type: 'object', properties: { command: { type: 'string' }, outcome: { type: 'string' } }, required: ['command', 'outcome'] } },
  },
  required: ['verdict', 'summary', 'resolved', 'problems', 'testsRun'],
}

const results = await pipeline(args.items, item => agent(
  `You are independently verifying fix commits for package ${item.id} of the kudurru v0.2.0 release. They were written by a fixer after a code review and were then integrated WITHOUT the usual verification pass, so nobody but their author has checked them.

Read ${SHARED}/AGENT-BRIEF.md section 5 for how to run tests on this shared machine (heavy runs through the wrapper). DO NOT EDIT, STAGE OR COMMIT ANYTHING. Checkout to read and run tests in: ${item.checkout} (${item.checkoutNote}).

The commits to verify, by subject (find them with \`cd ${item.checkout} && git log --oneline ${item.range}\`): ${item.subjects.join('; ')}.

The review findings they were meant to resolve, the fixer's own report and its commit list (original lane SHAs) are in ${item.contextFile} (JSON: findings[], fix.summary, fix.addressed[], fix.rebutted[]). The package specification is in ${SHARED}/${item.mapFile} (remaining[] id ${item.mapId}).

Do three things, with evidence for every statement:
1. For each finding, decide from the code and by running the relevant tests whether it is really resolved. A new test only counts if it would fail without the fix: check that by reading it against the pre-fix code (\`git show <sha>^:path\`) or by reasoning about what it asserts.
2. Review the fix commits themselves as new, unreviewed production changes: correctness on the main path and on failure paths (shutdown, reload, concurrent queries, a sink that never drains), data races, lost or double-counted events, goroutine leaks, and any guard, deadline or assertion they loosened.
3. Run the package's key tests at least twice under the race detector and report the real output.

Report only what you can support. Say plainly when the commits are sound.`,
  { label: `verify-fix:${item.id}`, phase: 'Verify', schema: SCHEMA }
).then(r => r ? { id: item.id, ...r } : { id: item.id, verdict: 'not-run' }))
return results
