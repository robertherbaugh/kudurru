"""One owner-approved cloud-sandbox historical calibration; no candidate timing."""
import datetime
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys

BASE = Path(__file__).resolve().parent
CONTROL = BASE.parent / 'cloud-benchmark-method'
ORIGINAL = BASE.parent / 'benchmark-reference-0cc9565e'
LATER = BASE.parent / 'benchmark-later-reference-9cc2ae04'
EXPECTED = '3ff60b9f21f9d2abe114eed418b2179d6f28032d'
FROZEN = BASE.parent / 'dedicated-benchmark-method'
PENDING = BASE.parent / 'release-shared-calibration-20261115/sandbox-host-pending.json'


def now():
    return datetime.datetime.now(datetime.timezone.utc).isoformat()


def save(name, value):
    with (BASE / name).open('x') as output:
        json.dump(value, output, indent=2)
        output.write('\n')


def snapshot():
    paths = ('/proc/stat', '/proc/loadavg', '/proc/meminfo', '/proc/pressure/cpu',
             '/proc/pressure/memory', '/proc/pressure/io', '/proc/self/cgroup')
    return {'at': now(), 'counters': {path: Path(path).read_text() for path in paths if Path(path).exists()},
            'processes': subprocess.check_output(['ps', '-eo', 'pid,ppid,comm,pcpu'], text=True)}


assert Path.cwd() == CONTROL
assert not (BASE / 'calibration').exists()
assert not (BASE / 'measurement-started.json').exists()
assert subprocess.check_output(['git', 'rev-parse', 'HEAD']).decode().strip() == EXPECTED
assert not subprocess.check_output(['git', 'status', '--porcelain', '--untracked-files=all'])
# Only the profile's name differs from the accepted collector implementation.
old = (FROZEN / 'hack/bench-dedicated.py').read_text()
new = (CONTROL / 'hack/bench-dedicated.py').read_text()
assert new == old.replace('CLASS = "dedicated-homelab-v1"', 'CLASS = "shared-cloud-sandbox-v1"')
for name in ('bench-paired.py', 'bench-reference.py', 'bench-release.py'):
    assert (CONTROL / 'hack' / name).read_bytes() == (FROZEN / 'hack' / name).read_bytes()
# Refuse to overlap unrelated builds/tests. This is a preflight, not a sample.
active = subprocess.run(['pgrep', '-x', 'go|compile|link|security.test'], stdout=subprocess.PIPE, text=True)
assert active.returncode == 1, 'unrelated local build/test is still active'
spec = importlib.util.spec_from_file_location('dedicated', CONTROL / 'hack/bench-dedicated.py')
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)
review = json.loads((BASE / 'owner-method-decision.json').read_text())
assert review['decision'] == 'accepted'
assert review['proposal_sha256'] == hashlib.sha256((BASE / 'owner-reviewed-proposal.md').read_bytes()).hexdigest()
pending = json.loads(PENDING.read_text())
identity = module.host_identity()
assert pending['identity'] == identity, 'sandbox identity changed since owner review'
assert identity['system']['/proc/self/cgroup'].strip() == '0::/conductor/workload'
save('host-before-review.json', pending)
pending.update(identity=identity, decision='accepted', reviewed_by=review['reviewed_by'],
    reviewed_at=review['reviewed_at'], review_evidence=review['review_evidence'],
    physical_host='Vercel cloud sandbox: physical host identity opaque; owner explicitly accepts guest-only continuity under the cloud amendment.',
    vm_id=identity['hostname'] + '; machine-id-sha256=' + identity['machine_id_sha256'],
    cpu_reservation='Eight exposed vCPUs; six Go processors; guest affinity fixed. Physical pinning, SMT placement and tenant sharing unavailable and explicitly owner-accepted.',
    isolation_evidence='Owner accepts the exact disclosed cloud-sandbox amendment; proposal SHA256=' + review['proposal_sha256'] + '. Unrelated builds/tests finish before timing; stable /conductor/workload cgroup; no physical exclusivity claimed.',
    guest_image='Existing Amazon Linux 2023 Vercel sandbox; owner accepts current guest image bound by captured os-release, kernel, machine and boot identity.')
module.validate_host(pending)
save('host-reviewed.json', pending)
argv = ['/usr/bin/python3', '-B', str(CONTROL / 'hack/bench-dedicated.py'), 'calibrate',
        '--host-record', str(BASE / 'host-reviewed.json'), '--reference', str(ORIGINAL),
        '--later-reference', str(LATER), '--output', str(BASE / 'calibration')]
save('measurement-started.json', {'at': now(), 'argv': argv, 'method_source': EXPECTED,
    'collector_base': 'fef3a1fde4cf8b847ba833d9e1f0710e7ec3f998',
    'collector_delta': 'Profile class metadata only: shared-cloud-sandbox-v1.',
    'orchestration_sha256': hashlib.sha256(Path(__file__).read_bytes()).hexdigest(),
    'scope': '40 historical native commands, five samples, six processors; no candidate measurement',
    'recording': 'v020-cloud-shared-20261115', 'release_qualified': False})
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
