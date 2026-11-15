"""Run one complete paired qualification bound to actual owner-approved limits."""
import datetime
import hashlib
import importlib.util
import json
from pathlib import Path
import subprocess
import sys

BASE=Path(__file__).resolve().parent
CONTROL=BASE.parent/'cloud-benchmark-method'
ORIGINAL=BASE.parent/'benchmark-reference-0cc9565e'
EXPECTED='3ff60b9f21f9d2abe114eed418b2179d6f28032d'
MANIFEST='3b38bf0389bb04e2ed9b0546d79e276ffdbf70090ae8588afa7c5fb2a0bf7266'

def now():
    return datetime.datetime.now(datetime.timezone.utc).isoformat()

def read(path):
    return json.loads(path.read_text())

def save(name,value):
    with (BASE/name).open('x') as output:
        json.dump(value,output,indent=2); output.write('\n')

def snapshot():
    paths=('/proc/stat','/proc/loadavg','/proc/meminfo','/proc/pressure/cpu','/proc/pressure/memory','/proc/pressure/io','/proc/self/cgroup')
    return {'at':now(),'counters':{path:Path(path).read_text() for path in paths if Path(path).exists()},
            'processes':subprocess.check_output(['ps','-eo','pid,ppid,comm,pcpu'],text=True)}

assert Path.cwd()==CONTROL
assert not (BASE/'qualification').exists() and not (BASE/'qualification-started.json').exists()
assert subprocess.check_output(['git','rev-parse','HEAD']).decode().strip()==EXPECTED
assert not subprocess.check_output(['git','status','--porcelain','--untracked-files=all'])
assert subprocess.run(['pgrep','-x','go|compile|link|security.test'],stdout=subprocess.PIPE).returncode==1
spec=importlib.util.spec_from_file_location('dedicated',CONTROL/'hack/bench-dedicated.py')
module=importlib.util.module_from_spec(spec); spec.loader.exec_module(module)
module.verify_calibration(BASE/'calibration')
assert module.runner.digest(BASE/'calibration/manifest.json')==MANIFEST
approval=read(BASE/'approval.json')
module.approved(approval,'calibration')
assert approval['calibration_manifest_sha256']==MANIFEST
module.validate_host(read(BASE/'host-reviewed.json'))
sequence=module.paired.interleaved_sequence()
assert len(sequence)==60 and sum(row[0]=='reference' for row in sequence)==20 and sum(row[0]=='candidate' for row in sequence)==40
argv=['/usr/bin/python3','-B',str(CONTROL/'hack/bench-dedicated.py'),'qualify',
    '--host-record',str(BASE/'host-reviewed.json'),'--reference',str(ORIGINAL),
    '--calibration',str(BASE/'calibration'),'--approval',str(BASE/'approval.json'),
    '--output',str(BASE/'qualification')]
save('qualification-plan.json',{'method_source':EXPECTED,'runtime_source':'5e19a32bfb4d62f8524218f0e1cec3df6806eaa0',
    'reference':module.reference.REFERENCE,'sequence':sequence,'runner_class':module.CLASS,
    'procs':6,'samples':5,'regression_percent':20,'calibration_manifest_sha256':MANIFEST,
    'approval_sha256':module.runner.digest(BASE/'approval.json'),'argv':argv,'release_qualified':False})
save('qualification-started.json',{'at':now(),'method_source':EXPECTED,'argv':argv,
    'orchestration_sha256':hashlib.sha256(Path(__file__).read_bytes()).hexdigest(),'release_qualified':False})
save('qualification-guest-before.json',snapshot())
print('PAIRED_QUALIFICATION_START',now(),flush=True)
code=subprocess.run(argv,check=False).returncode
save('qualification-guest-after.json',snapshot())
save('qualification-execution-result.json',{'at':now(),'exit_code':code,'release_qualified':False})
print('PAIRED_QUALIFICATION_COMPLETE',code,flush=True)
sys.exit(code)
