"""Read-only validation of the approved cloud-sandbox historical calibration."""
import hashlib
import importlib.util
import json
from pathlib import Path
import subprocess
import sys
import tarfile

BASE = Path(__file__).resolve().parent
GUEST = BASE
CAL = GUEST / 'calibration'
REPO = BASE.parent.parent
METHOD = REPO / '.context/cloud-benchmark-method'
EXPECTED_CONTROL = '3ff60b9f21f9d2abe114eed418b2179d6f28032d'


def read(path):
    return json.loads(path.read_text())


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def source_manifest(revision):
    process = subprocess.Popen(['git', 'archive', revision], cwd=REPO, stdout=subprocess.PIPE)
    entries = {}
    with tarfile.open(fileobj=process.stdout, mode='r|') as archive:
        for item in archive:
            if item.isfile():
                entries[item.name] = hashlib.sha256(archive.extractfile(item).read()).hexdigest()
    assert process.wait() == 0
    return entries


assert subprocess.check_output(['git', 'rev-parse', 'HEAD'], cwd=METHOD).decode().strip() == EXPECTED_CONTROL
spec = importlib.util.spec_from_file_location('dedicated', METHOD / 'hack/bench-dedicated.py')
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)
plan = read(CAL / 'plan.json')
manifest = read(CAL / 'manifest.json')
assert module.inventory(CAL) == manifest['files'], 'manifest inventory/hash differs'
assert plan['sources'] == {'original': module.reference.REFERENCE, 'later': module.LATER}
assert plan['control'] == EXPECTED_CONTROL
assert (plan['procs'], plan['samples']) == (6, 5)
assert plan['commands'] == module.runner.commands()
assert plan['sequence'] == [[scope, sample] for sample in range(1, 6) for scope in module.runner.SUITES]
assert plan['legacy_limits'] == {name: digest(METHOD / path) for name, path in module.runner.LIMIT_FILES.items()}
assert read(CAL / 'host.json') == read(GUEST / 'host-reviewed.json')
assert read(GUEST / 'owner-method-decision.json')['decision'] == 'accepted'
assert read(GUEST / 'measurement-started.json')['method_source'] == EXPECTED_CONTROL
assert read(GUEST / 'measurement-started.json')['orchestration_sha256'] == digest(GUEST / 'run-historical-calibration.py')
source_counts = {}
for name, revision in {**plan['sources'], 'control': plan['control']}.items():
    actual = source_manifest(revision)
    assert actual == read(CAL / (name + '-source.json')), 'source manifest differs: ' + name
    source_counts[name] = len(actual)
receipts = []
for sample in range(1, 6):
    for scope, argv in module.runner.commands().items():
        label = f'{scope}-{sample}'
        receipt = read(CAL / (label + '.json'))
        assert receipt['argv'] == argv
        assert receipt['stop_reason'] is None and receipt['group_reaped']
        assert receipt['output_bytes'] == (CAL / (label + '.log')).stat().st_size
        receipts.append({'label': label, **receipt})
assert len(receipts) == len({item['pid'] for item in receipts}) == 40
reports = {}
for report in module.runner.REPORTS:
    receipt = read(CAL / (report + '-shape.json'))
    assert receipt['stop_reason'] is None and receipt['group_reaped']
    assert receipt['output_bytes'] == (CAL / (report + '-shape.log')).stat().st_size
    values = module.vectors((CAL / (report + '.txt')).read_text())
    # The historical blocking report also emits its informational memory entry
    # count. The committed baseline omits it and limits.txt explicitly ignores
    # entries. Validate that precise extra rather than accepting arbitrary rows.
    expected = module.expected_metrics()[report]
    if report == 'blocking-zone':
        expected = expected | {('BenchmarkMemory/3M', 'entries')}
    assert set(values) == expected
    reports[report] = {'exit_code': receipt['child_exit_code'], 'metrics': len(values)}
result = read(CAL / 'result.json')
assert result['measurement_complete'] and result['release_qualified'] is False
assert read(GUEST / 'execution-result.json')['exit_code'] == result['exit_code']
eligible = result['exit_code'] == 0
if eligible:
    module.verify_calibration(CAL)
    review = read(GUEST / 'approval-template.json')
    assert review['calibration_manifest_sha256'] == digest(CAL / 'manifest.json')
    assert review['decision'] == 'pending'
else:
    assert not (GUEST / 'approval-template.json').exists()
    assert not list(CAL.glob('*-limits.txt'))
summary = {'manifest_sha256': digest(CAL / 'manifest.json'), 'files_verified': len(manifest['files']),
           'source_paths_verified': source_counts, 'native_commands': len(receipts),
           'native_failures': [item['label'] for item in receipts if item['child_exit_code']],
           'minimum_disk_free_bytes': min(item['minimum_free_bytes'] for item in receipts),
           'shape_product_gates': reports, 'eligible_for_limit_review': eligible,
           'exit_code': result['exit_code'], 'release_qualified': False}
(BASE / 'verification.json').write_text(json.dumps(summary, indent=2) + '\n')
print(json.dumps(summary, indent=2))
