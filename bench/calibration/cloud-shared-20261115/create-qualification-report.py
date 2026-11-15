"""Summarize the completed recording without launching benchmark processes."""
import csv
import importlib.util
import json
from pathlib import Path
import re
import statistics

BASE = Path(__file__).resolve().parent
METHOD = BASE.parent / 'cloud-benchmark-method'
PAIR = BASE / 'qualification/paired'
spec = importlib.util.spec_from_file_location('dedicated', METHOD / 'hack/bench-dedicated.py')
m = importlib.util.module_from_spec(spec)
spec.loader.exec_module(m)
verified = json.loads((BASE / 'qualification-verification.json').read_text())
assert verified['performance_pass'] and verified['native_commands'] == 60
fixed = []
vectors = []
for role, reports in (('reference', m.reference.REPORTS), ('candidate', m.runner.REPORTS)):
    for report in reports:
        measured = m.vectors((PAIR / role / (report + '.txt')).read_text())
        for (name, unit), values in sorted(measured.items()):
            median = statistics.median(values)
            vectors.append({'role': role, 'report': report, 'name': name, 'unit': unit,
                            **{f'sample_{i}': value for i, value in enumerate(values, 1)},
                            'median': median})
        if report != 'blocking-zone':
            continue
        for line in (METHOD / 'bench/limits.txt').read_text().splitlines():
            fields = line.split()
            if not fields or fields[0] != 'limit':
                continue
            _, pattern, unit, bound = fields
            for (name, metric_unit), values in sorted(measured.items()):
                if unit == metric_unit and re.search(pattern, name):
                    median = statistics.median(values)
                    fixed.append({'role': role, 'name': name, 'unit': unit, 'values': values,
                                  'median': median, 'limit': float(bound), 'pass': median <= float(bound)})
assert fixed and all(row['pass'] for row in fixed)
(BASE / 'qualification-fixed-product-metrics.json').write_text(json.dumps(fixed, indent=2) + '\n')
with (BASE / 'qualification-metric-vectors.csv').open('w', newline='') as output:
    writer = csv.DictWriter(output, fieldnames=list(vectors[0]), lineterminator='\n')
    writer.writeheader()
    writer.writerows(vectors)
lines = [
    '# Paired performance qualification: PASS', '',
    'The one owner-authorized recording ran November 15, 2026, from 21:08:23 to '
    '21:34:04 UTC. All 60 native commands, ten absolute gates and three eligible '
    'relative comparisons pass. This satisfies the performance gate for runtime '
    '`5e19a32bfb4d62f8524218f0e1cec3df6806eaa0` under the approved '
    '`shared-cloud-sandbox-v1` method and limits.', '',
    'This scoped result does not grant overall release acceptance. The benchmark '
    'tools intentionally retain `release_qualified: false`. Independent GitHub '
    'review, remaining feature acceptance and release/promotion decisions stay '
    'separate. No merge, tag, release or production change is performed.', '',
    '## Approved inputs', '',
    '- Owner Robert Herbaugh explicitly answered “Approve these limits and run '
    'qualification.” [APPROVAL.md](APPROVAL.md) and [approval.json](approval.json) '
    'retain the actual decision against review commit '
    '`071cdbcb4c1fe1b29de66346d171d9bd5b846970`.',
    '- Accepted calibration manifest SHA256: '
    '`3b38bf0389bb04e2ed9b0546d79e276ffdbf70090ae8588afa7c5fb2a0bf7266`.',
    '- Approval SHA256: `291c461fd91bfc3a2e235b79f6f133e368ce7249a7342d7c1e301878adaa03e5`.',
    '- Frozen candidate/control `3ff60b9f21f9d2abe114eed418b2179d6f28032d`; '
    'runtime matches `5e19a32b` except the seven benchmark tooling/documentation '
    'files listed in [candidate-tree-delta.txt](candidate-tree-delta.txt). '
    'Historical reference `0cc9565e9a878f8223fc1cee5201eb7eca8aacd2`.',
    '- Same eight-vCPU sandbox and Go 1.27.4, CGO disabled, six benchmark '
    'processors and five fresh processes per scope. Physical host identity, '
    'pinning, SMT placement and competing tenants are unavailable and were '
    'explicitly accepted. Guest identity/cgroup/toolchain/storage, sources, '
    'calibration and approval remain guarded around each process.',
    '- Original workloads, durations, fixed product bounds, 20% relative '
    'threshold, noise floors and reference eligibility remain unchanged. '
    'The 138 separately approved historical hardware bounds are used exactly; '
    '63 are tighter, 35 looser and 40 unchanged from legacy profiles.', '',
    '## Verification', '',
    'The independent checker verifies all 165 qualification artifact hashes, '
    '60 exact command receipts in declared order with distinct process IDs, '
    'report concatenation, all ten absolute gates and three relative checks. '
    'Source maps match Git archives: 406 reference and 5,349 candidate paths. '
    'No native failure, resource stop or unreaped descendant is recorded. '
    'Minimum free disk is 10,982,465,536 bytes.', '',
    'Qualification artifact-manifest SHA256: '
    '`2da1492cdb2c523561bb4995796b5188e23e0394c5ab25e7159890570987184f`.', '',
    '| Check | Result |', '| --- | --- |',
    '| Native reference / candidate commands | 20 / 40 PASS |',
    '| Reference absolute gates | blocking/zone, cache, upstream PASS |',
    '| Candidate absolute gates | blocking/zone, cache, upstream, UDP, DoT, QUIC, persistence PASS |',
    '| Relative comparisons | blocking/zone, cache, upstream PASS; reference eligible |', '',
    'All measured five-value vectors and medians are in '
    '[qualification-metric-vectors.csv](qualification-metric-vectors.csv). '
    'The [raw recording](qualification/paired), '
    '[verification result](qualification-verification.json), '
    '[artifact manifest](qualification-evidence-manifest.json), exact plans, '
    'before/after guest counters and console log are retained. The archived '
    'orchestration and verification scripts preserve the original workspace '
    'paths; they document this execution and must not be invoked as a retry.', '',
    '## Fixed product results', '',
    'The table shows medians of the five fresh-process values. Every matched '
    'fixed product metric passes on both sources. Exact vectors are retained '
    'in [qualification-fixed-product-metrics.json](qualification-fixed-product-metrics.json).', '',
    '| Metric | Unit | Reference | Candidate | Fixed maximum |',
    '| --- | --- | ---: | ---: | ---: |']
by_metric = {}
for row in fixed:
    key = (row['name'], row['unit'], row['limit'])
    by_metric.setdefault(key, {})[row['role']] = row['median']
for (name, unit, bound), roles in by_metric.items():
    assert set(roles) == {'reference', 'candidate'}
    lines.append(f"| `{name}` | {unit} | {roles['reference']:g} | {roles['candidate']:g} | {bound:g} |")
lines += ['',
    '## Retained failures and scope', '',
    'The original hosted D412 run `43690014307` and the shared homelab calibration '
    'remain failed. Their recordings were not replaced or reclassified. The '
    'owner separately approved this sandbox method, then reviewed actual '
    'historical calibration values before candidate timing. This recording ran '
    'once without sample replacement, limit relaxation or baseline refresh.', '',
    'The disposable homelab VM 20027 and its exact seed ISO were removed after '
    'its failed evidence was archived. No production instance or existing guest '
    'was changed. Previously completed IAM/release-artifact proofs are separate '
    'and were not repeated during this measurement.', '']
(BASE / 'QUALIFICATION.md').write_text('\n'.join(lines))
print(f'Prepared report, {len(vectors)} metric vectors and {len(fixed)} fixed-product checks')
