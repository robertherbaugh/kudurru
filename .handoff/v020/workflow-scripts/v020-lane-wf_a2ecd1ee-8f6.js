export const meta = {
  name: 'v020-lane',
  description: 'Implement, adversarially review and fix a sequence of v0.2.0 packages in one lane worktree',
  phases: [
    { title: 'Implement', detail: 'one implementer per package, sequential in the lane worktree' },
    { title: 'Review', detail: 'independent adversarial review of each package' },
    { title: 'Fix', detail: 'address blocker and major findings' },
    { title: 'Verify', detail: 'confirm fixes for blocker findings' },
  ],
}

const { lane, worktree, packages } = args
const SHARED = '/home/vercel-sandbox/kudurru/.context/v020-final-20261110'

const IMPL_SCHEMA = {
  type: 'object',
  properties: {
    status: { type: 'string', enum: ['done', 'partial', 'blocked'] },
    summary: { type: 'string', description: 'What now happens, in plain prose (8-20 sentences)' },
    baseCommit: { type: 'string', description: 'git rev-parse HEAD before your first commit' },
    commits: {
      type: 'array',
      items: {
        type: 'object',
        properties: { sha: { type: 'string' }, subject: { type: 'string' }, generated: { type: 'boolean' } },
        required: ['sha', 'subject', 'generated'],
      },
    },
    testsRun: {
      type: 'array',
      items: {
        type: 'object',
        properties: { command: { type: 'string' }, outcome: { type: 'string' }, log: { type: 'string' } },
        required: ['command', 'outcome'],
      },
    },
    decisionsMade: { type: 'array', items: { type: 'string' } },
    remaining: { type: 'array', items: { type: 'string' }, description: 'Precisely what is NOT done from the package scope, and why' },
    uiNeeds: { type: 'array', items: { type: 'string' }, description: 'What a later dashboard lane must build on top of this (API fields, capability flags, routes)' },
    newDependencies: { type: 'array', items: { type: 'string' } },
    notesForIntegrator: { type: 'string' },
  },
  required: ['status', 'summary', 'baseCommit', 'commits', 'testsRun', 'remaining', 'notesForIntegrator'],
}

const REVIEW_SCHEMA = {
  type: 'object',
  properties: {
    verdict: { type: 'string', enum: ['accept', 'accept-with-fixes', 'reject'] },
    summary: { type: 'string' },
    findings: {
      type: 'array',
      items: {
        type: 'object',
        properties: {
          severity: { type: 'string', enum: ['blocker', 'major', 'minor', 'nit'] },
          file: { type: 'string' },
          line: { type: 'integer' },
          issue: { type: 'string' },
          evidence: { type: 'string', description: 'Concrete inputs or state that produce the wrong result, or the command output that shows it' },
          suggestedFix: { type: 'string' },
        },
        required: ['severity', 'file', 'issue', 'evidence', 'suggestedFix'],
      },
    },
    acceptanceCoverage: {
      type: 'array',
      items: {
        type: 'object',
        properties: { criterion: { type: 'string' }, covered: { type: 'boolean' }, by: { type: 'string' } },
        required: ['criterion', 'covered', 'by'],
      },
    },
    guardsWeakened: { type: 'array', items: { type: 'string' }, description: 'Existing guards, deadlines, assertions or tests that the change loosened, skipped or deleted' },
  },
  required: ['verdict', 'summary', 'findings', 'acceptanceCoverage', 'guardsWeakened'],
}

const FIX_SCHEMA = {
  type: 'object',
  properties: {
    status: { type: 'string', enum: ['fixed', 'partial', 'nothing-to-fix'] },
    summary: { type: 'string' },
    commits: {
      type: 'array',
      items: {
        type: 'object',
        properties: { sha: { type: 'string' }, subject: { type: 'string' }, generated: { type: 'boolean' } },
        required: ['sha', 'subject', 'generated'],
      },
    },
    addressed: { type: 'array', items: { type: 'object', properties: { finding: { type: 'string' }, action: { type: 'string' } }, required: ['finding', 'action'] } },
    rebutted: { type: 'array', items: { type: 'object', properties: { finding: { type: 'string' }, why: { type: 'string' } }, required: ['finding', 'why'] } },
    testsRun: { type: 'array', items: { type: 'object', properties: { command: { type: 'string' }, outcome: { type: 'string' } }, required: ['command', 'outcome'] } },
  },
  required: ['status', 'summary', 'commits', 'addressed', 'rebutted', 'testsRun'],
}

const VERIFY_SCHEMA = {
  type: 'object',
  properties: {
    resolved: { type: 'array', items: { type: 'string' } },
    unresolved: { type: 'array', items: { type: 'object', properties: { finding: { type: 'string' }, why: { type: 'string' } }, required: ['finding', 'why'] } },
    newProblems: { type: 'array', items: { type: 'string' } },
    summary: { type: 'string' },
  },
  required: ['resolved', 'unresolved', 'newProblems', 'summary'],
}

function mapRefs(p) {
  return p.mapRefs.map(r => `- ${SHARED}/${r.file} : remaining[] entries with id ${r.ids.join(', ')}`).join('\n')
}

function implPrompt(p, prior) {
  return `You are implementing one work package for the kudurru v0.2.0 release.

FIRST read ${SHARED}/AGENT-BRIEF.md completely and follow it exactly (workspace, forbidden files, commit protocol, heavy-test lock, outputs).

Lane: ${lane}
Worktree (the only place you edit code): ${worktree}   (branch wip/v020/${lane})
Package ID (use as <PKG> for $SHARED/out and $SHARED/evidence): ${p.id}
Title: ${p.title}
Decision number assigned to this package: ${p.decision || 'none (do not write decision.md unless you make a design decision that needs recording; if you do, write it without a number and say so)'}

Specification (map entries; read the whole map file, then your entries):
${mapRefs(p)}

${p.notes ? 'Additional direction from the integrator:\n' + p.notes + '\n' : ''}
${prior.length ? 'Earlier packages already committed in this lane during this run (your work builds on them; read their commits with git log/git show):\n' + prior.map(x => `- ${x.id}: ${x.status}; commits ${x.commits.map(c => c.sha.slice(0, 10)).join(', ') || 'none'}; remaining: ${(x.remaining || []).join(' | ') || 'nothing'}`).join('\n') + '\n' : ''}
${(args.interruptedImpl || []).includes(p.id) ? 'IMPORTANT: an earlier implementer of THIS package was interrupted before it could report (the run was cut off; the machine has since restarted, so no test process survives). Its unfinished work is still here: possibly commits on the lane branch after the earlier packages listed above, uncommitted changes and new files in the worktree (`git status --short`, `git diff`), logs under ' + SHARED + '/evidence/' + p.id + '/ and draft files under ' + SHARED + '/out/' + p.id + '/. Treat all of it as your own unfinished work: read it, understand the design it was following, check that it builds and that its tests pass, finish what is missing, then commit and report. Do not discard or rewrite it without a concrete reason. Your baseCommit is the last commit of the previous package listed above (or 33ee04bc if there is none); report every commit of this package after it, including the earlier implementer\'s.\n\n' : ''}Work method:
1. Run \`cd ${worktree} && git status --short && git log --oneline -5\` and record the base commit. The tree must be clean; if it is not, inspect what is there (an earlier agent in this lane may have left uncommitted work), decide whether it belongs to a finished package, and do not discard anything you cannot attribute.
2. Read the specification, the cited tracker sections and decision rows, and the neighboring code and tests until you can state the design in a few sentences. Prefer the smallest complete change that follows existing patterns.
3. Implement with tests that prove the acceptance criteria through the real seams. Run them (heavy runs through the lock wrapper), keep numbered logs.
4. Commit per the brief (source commits, then at most one chore(gen) commit). Write the out/ files.
5. Return the structured result. Be exact about what is not done.

You have no one to ask. Make engineering decisions yourself and record them. Do not stop early: a package is only useful to the release when it is mounted, tested and committed.`
}

function reviewPrompt(p, impl) {
  return `You are an independent, adversarial reviewer for one work package of the kudurru v0.2.0 release. Your job is to find what is wrong, missing or unproven before the integrator cherry-picks it. You did not write this code; assume nothing works until you have seen why it does.

Read ${SHARED}/AGENT-BRIEF.md first: it is the contract the implementer had to follow. YOU MUST NOT EDIT, STAGE OR COMMIT ANYTHING. You may run tests and builds in the lane worktree following section 5 of the brief (heavy runs through the lock wrapper; write logs to ${SHARED}/evidence/${p.id}/review-*.log).

Lane worktree: ${worktree}
Package: ${p.id} — ${p.title}
Specification:
${mapRefs(p)}
${p.notes ? 'Integrator direction given to the implementer:\n' + p.notes + '\n' : ''}
Implementer's report:
- status: ${impl.status}
- base commit: ${impl.baseCommit}
- commits: ${impl.commits.map(c => `${c.sha.slice(0, 12)} ${c.generated ? '[generated] ' : ''}${c.subject}`).join('; ')}
- summary: ${impl.summary}
- stated remainder: ${(impl.remaining || []).join(' | ') || 'none'}
- tests the implementer says passed: ${impl.testsRun.map(t => `${t.command} => ${t.outcome}`).join(' ;; ').slice(0, 3000)}

Review the actual diff (\`cd ${worktree} && git log --stat ${impl.baseCommit}..HEAD\` and \`git show <sha>\`), not the report. Check, with evidence for every finding:
1. Acceptance: every criterion in the specification entries is implemented in the production path and proven by a test that would fail without the change. Name criteria that are unproven or only unit-tested when a mounted behavior was required.
2. Correctness: wrong results, lost updates, missing error handling on real failure paths, restart and failover behavior, lease/authority fencing, compare-and-swap usage, context/deadline handling, goroutine leaks, data races. Trace at least the main success path and two failure paths through the code by hand.
3. Security: authorization on every new route or command (permission declared and enforced, scoped tokens, original credential rechecked at commit where neighbors do), secret handling (nothing logged, returned or persisted in clear that neighbors keep sealed), input bounds, SSRF or path traversal in anything that takes a URL or path.
4. Guards: any existing guard, deadline, assertion or test that was loosened, skipped or deleted (\`git diff ${impl.baseCommit}..HEAD -- '*_test.go'\` for changed existing tests). List each in guardsWeakened.
5. Contract with the integrator: forbidden files untouched; generated artifacts only in a final chore(gen) commit; out/${p.id}/{ledger.md,changelog.md${p.decision ? ',decision.md' : ''}} exist and match what the code does (no overclaiming); American English; sign-off and Co-Authored-By trailer present.
6. Run the package's key tests yourself at least once (and twice for anything concurrent) and report real output. If a test is flaky, that is a finding.

Severity: blocker = wrong behavior, security defect, data loss, or an acceptance criterion claimed but not met; major = likely defect or missing proof for a required behavior; minor = should fix but does not block; nit = style. Do not pad the list: report only what you can support with evidence, and say plainly when the work is sound.`
}

function fixPrompt(p, impl, review) {
  const items = review.findings.filter(f => f.severity !== 'nit')
  return `You are fixing review findings for one work package of the kudurru v0.2.0 release.

Read ${SHARED}/AGENT-BRIEF.md first and follow it exactly. Lane worktree: ${worktree}. Package: ${p.id} — ${p.title}. Specification:
${mapRefs(p)}

The implementation is committed (${impl.baseCommit}..HEAD). An independent reviewer returned verdict "${review.verdict}": ${review.summary}

Findings to resolve (blocker and major are mandatory; fix minor ones when the fix is small and safe):
${items.map((f, i) => `${i + 1}. [${f.severity}] ${f.file}${f.line ? ':' + f.line : ''} — ${f.issue}\n   evidence: ${f.evidence}\n   suggested: ${f.suggestedFix}`).join('\n')}

Guards the reviewer says were weakened: ${review.guardsWeakened.join(' | ') || 'none'}
Acceptance criteria the reviewer says are not covered: ${review.acceptanceCoverage.filter(a => !a.covered).map(a => a.criterion + ' (' + a.by + ')').join(' | ') || 'none'}

For each finding: reproduce or confirm it by reading the code, then fix it with a test that fails before the fix, or rebut it with concrete evidence if the reviewer is wrong. Restore any weakened guard unless you can show the guard itself was wrong. Add new source commits on top (never amend or rewrite existing commits); if generated artifacts change, add a new final chore(gen) commit. Update ${SHARED}/out/${p.id}/ledger.md and changelog.md so they match the final behavior. Keep logs under ${SHARED}/evidence/${p.id}/fix-*.log.${(args.interruptedFix || []).includes(p.id) ? '\n\nIMPORTANT: an earlier fixer for this package was interrupted before it could report. Its commits are already on the lane branch after the implementation commits listed above (see `git log --oneline ' + impl.baseCommit + '..HEAD`), the worktree is clean, and its logs are under ' + SHARED + '/evidence/' + p.id + '/. Read those commits first, decide for each finding whether it is already resolved, and continue from there instead of redoing finished work. Report all fix commits (the earlier fixer\'s and yours) in your result.' : ''}${(args.interruptedFix2 || []).includes(p.id) ? '\n\nIMPORTANT: an earlier fixer for this package was interrupted before it could report (the run was cut off; the machine has since restarted). Its unfinished work is still here: possibly fix commits on the lane branch after the implementation commits listed above (see `git log --oneline ' + impl.baseCommit + '..HEAD`), uncommitted changes and new files in the worktree (`git status --short`, `git diff`), and logs under ' + SHARED + '/evidence/' + p.id + '/. Treat it as your own unfinished work: read it first, decide for each finding whether it is already resolved, check that the uncommitted changes build and pass, finish and commit them, and continue with what is left. Do not discard it without a concrete reason. Report all fix commits (the earlier fixer\'s and yours) in your result.' : ''}`
}

function verifyPrompt(p, impl, review, fix) {
  const items = review.findings.filter(f => p.verifyFix ? f.severity !== 'nit' : (f.severity === 'blocker' || f.severity === 'major'))
  return `You are verifying that review findings were actually resolved for package ${p.id} (${p.title}) of the kudurru v0.2.0 release. Read ${SHARED}/AGENT-BRIEF.md section 5 for how to run tests on this shared machine. DO NOT EDIT, STAGE OR COMMIT ANYTHING. Lane worktree: ${worktree}.

Original blocker/major findings:
${items.map((f, i) => `${i + 1}. [${f.severity}] ${f.file}${f.line ? ':' + f.line : ''} — ${f.issue} (evidence: ${f.evidence})`).join('\n')}

The fixer reports: ${fix.summary}
Fix commits: ${fix.commits.map(c => c.sha.slice(0, 12) + ' ' + c.subject).join('; ') || 'none'}
Addressed: ${fix.addressed.map(a => a.finding + ' => ' + a.action).join(' ;; ')}
Rebutted: ${fix.rebutted.map(a => a.finding + ' => ' + a.why).join(' ;; ') || 'none'}

For each original finding decide from the code and by running the relevant tests whether it is resolved, correctly rebutted, or still open. Also look at the fix commits themselves (\`cd ${worktree} && git show <sha>\`) for new defects or weakened guards. Default to "unresolved" when you cannot confirm.`
}

const results = []
const prior = []
function opts(p, role, base) {
  const m = p.models && p.models[role]
  return m ? { ...base, model: m } : base
}
for (const p of packages) {
  const impl = await agent(implPrompt(p, prior), opts(p, 'impl', { label: `impl:${p.id}`, phase: 'Implement', schema: IMPL_SCHEMA }))
  if (!impl) {
    log(`${p.id}: implementer returned nothing; stopping the lane here`)
    results.push({ id: p.id, impl: null })
    break
  }
  let review = null, fix = null, verify = null
  if (impl.commits.length) {
    review = await agent(reviewPrompt(p, impl), opts(p, 'review', { label: `review:${p.id}`, phase: 'Review', schema: REVIEW_SCHEMA }))
    if (!review && args.fallbackModel) {
      log(`${p.id}: reviewer returned nothing; retrying on ${args.fallbackModel}`)
      review = await agent(reviewPrompt(p, impl), { label: `review2:${p.id}`, phase: 'Review', schema: REVIEW_SCHEMA, model: args.fallbackModel })
    }
    if (!review) {
      log(`${p.id}: no review could run; stopping the lane here`)
      results.push({ id: p.id, impl, review: null })
      break
    }
    const must = review.findings.filter(f => f.severity === 'blocker' || f.severity === 'major')
    const weakened = review.guardsWeakened.length
    if (must.length || weakened || review.findings.some(f => f.severity === 'minor')) {
      fix = await agent(fixPrompt(p, impl, review), opts(p, 'fix', { label: `fix:${p.id}`, phase: 'Fix', schema: FIX_SCHEMA }))
      if (!fix) {
        log(`${p.id}: fixer returned nothing; stopping the lane here`)
        results.push({ id: p.id, impl, review, fix: null })
        break
      }
      if (must.length || (p.verifyFix && fix.commits.length)) {
        verify = await agent(verifyPrompt(p, impl, review, fix), opts(p, 'verify', { label: `verify:${p.id}`, phase: 'Verify', schema: VERIFY_SCHEMA }))
        if (!verify && args.fallbackModel) {
          verify = await agent(verifyPrompt(p, impl, review, fix), { label: `verify2:${p.id}`, phase: 'Verify', schema: VERIFY_SCHEMA, model: args.fallbackModel })
        }
      }
    }
  }
  const commits = [...impl.commits, ...((fix && fix.commits) || [])]
  log(`${p.id}: ${impl.status}; review ${review ? review.verdict : 'n/a'}; ${commits.length} commits; unresolved ${verify ? verify.unresolved.length : 0}`)
  results.push({ id: p.id, impl, review, fix, verify })
  prior.push({ id: p.id, status: impl.status, commits, remaining: impl.remaining })
}
return { lane, results }
