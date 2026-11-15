"""One predeclared historical calibration, under a stable guest service cgroup."""
import datetime
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys

BASE = Path('/var/lib/kudurru-benchmark/v020-shared-20261115')
CONTROL = Path('/home/benchmark/kudurru/control')
HOST = Path('/var/lib/kudurru-benchmark/host.json')
EXPECTED = 'fef3a1fde4cf8b847ba833d9e1f0710e7ec3f998'


def now():
    return datetime.datetime.now(datetime.timezone.utc).isoformat()


def save(name, value):
    with (BASE / name).open('x') as output:
        json.dump(value, output, indent=2)
        output.write('\n')


def snapshot():
    paths = ('/proc/stat', '/proc/loadavg', '/proc/meminfo', '/proc/pressure/cpu',
             '/proc/pressure/memory', '/proc/pressure/io', '/proc/self/cgroup')
    return {'at': now(), 'counters': {path: Path(path).read_text() for path in paths if Path(path).exists()}}


assert Path.cwd() == CONTROL
assert not (BASE / 'calibration').exists()
assert not (BASE / 'measurement-started.json').exists()
assert subprocess.check_output(['git', 'rev-parse', 'HEAD']).decode().strip() == EXPECTED
assert not subprocess.check_output(['git', 'status', '--porcelain', '--untracked-files=all'])
spec = importlib.util.spec_from_file_location('dedicated', CONTROL / 'hack/bench-dedicated.py')
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)
review = json.loads((BASE / 'owner-host-decision.json').read_text())
assert review['decision'] == 'accepted' and review['method_source'] == EXPECTED
previous = json.loads(HOST.read_text())
save('host-before-shared-approval.json', previous)
identity = module.host_identity()
for key in previous['identity']:
    if key != 'system':
        assert previous['identity'][key] == identity[key], 'unexpected host change: ' + key
for key in previous['identity']['system']:
    if key != '/proc/self/cgroup':
        assert previous['identity']['system'][key] == identity['system'][key], 'unexpected system change: ' + key
assert identity['system']['/proc/self/cgroup'].strip() == '0::/system.slice/kudurru-benchmark.service'
previous.update(identity=identity, decision='accepted', reviewed_by=review['reviewed_by'],
                reviewed_at=review['reviewed_at'], review_evidence=review['review_evidence'],
                isolation_evidence='Owner accepts disclosed physical CPU sharing on VM20027; other guests have unrestricted affinity; no exclusive reservation claimed. Stable guest kudurru-benchmark.service execution cgroup.',
                guest_image='Ubuntu24.04 official image SHA256 04e8f2461a16a44e78a370284471f29616cdd9baf1893df25807b71f985b6c94')
module.runner.save(HOST, previous)
module.validate_host(previous)
save('host-reviewed.json', previous)
argv = ['/usr/bin/python3', '-B', str(CONTROL / 'hack/bench-dedicated.py'), 'calibrate',
        '--host-record', str(HOST), '--reference', '/home/benchmark/kudurru/reference',
        '--later-reference', '/home/benchmark/kudurru/later-reference', '--output', str(BASE / 'calibration')]
save('measurement-started.json', {'at': now(), 'argv': argv, 'method_source': EXPECTED,
     'orchestration_sha256': hashlib.sha256(Path(__file__).read_bytes()).hexdigest(),
     'scope': '40 historical native commands, five samples, six processors; no candidate measurement',
     'recording': 'v020-shared-20261115', 'release_qualified': False})
save('guest-before.json', snapshot())
print('HISTORICAL_CALIBRATION_START', now(), flush=True)
code = subprocess.run(argv, check=False).returncode
save('guest-after.json', snapshot())
save('execution-result.json', {'at': now(), 'exit_code': code, 'release_qualified': False})
if code == 0:
    module.verify_calibration(BASE / 'calibration')
    subprocess.run(['/usr/bin/python3', '-B', str(CONTROL / 'hack/bench-dedicated.py'), 'review-template',
        '--calibration', str(BASE / 'calibration'), '--output', str(BASE / 'approval-template.json')], check=True)
    print('CALIBRATION_COMPLETE_AWAITING_LIMIT_REVIEW', flush=True)
else:
    print('CALIBRATION_FAILED_OR_HELD_NO_CANDIDATE_AUTHORIZED', flush=True)
sys.exit(code)
