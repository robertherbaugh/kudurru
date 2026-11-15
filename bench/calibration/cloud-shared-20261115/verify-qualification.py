"""Read-only check of one completed paired recording and its approved inputs."""
import hashlib
import importlib.util
import json
from pathlib import Path
import subprocess
import tarfile

BASE=Path(__file__).resolve().parent
ROOT=BASE.parent.parent
CONTROL=BASE.parent/'cloud-benchmark-method'
CAL=BASE/'calibration'
QUAL=BASE/'qualification'
PAIR=QUAL/'paired'
HEAD='3ff60b9f21f9d2abe114eed418b2179d6f28032d'
CALHASH='3b38bf0389bb04e2ed9b0546d79e276ffdbf70090ae8588afa7c5fb2a0bf7266'

def read(path):
    return json.loads(path.read_text())

def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()

def source_manifest(revision):
    process=subprocess.Popen(['git','archive',revision],cwd=ROOT,stdout=subprocess.PIPE)
    result={}
    with tarfile.open(fileobj=process.stdout,mode='r|') as archive:
        for item in archive:
            if item.isfile():
                result[item.name]=hashlib.sha256(archive.extractfile(item).read()).hexdigest()
    assert process.wait()==0
    return result

assert subprocess.check_output(['git','rev-parse','HEAD'],cwd=CONTROL).decode().strip()==HEAD
spec=importlib.util.spec_from_file_location('dedicated',CONTROL/'hack/bench-dedicated.py')
m=importlib.util.module_from_spec(spec); spec.loader.exec_module(m)
m.verify_calibration(CAL)
assert digest(CAL/'manifest.json')==CALHASH
approval=read(BASE/'approval.json')
m.approved(approval,'calibration')
assert approval['calibration_manifest_sha256']==CALHASH
host=read(BASE/'host-reviewed.json')
assert host==read(CAL/'host.json')
m.validate_host(host)
review=read(QUAL/'dedicated-review.json')
assert review['approval']==approval and review['approval_sha256']==digest(BASE/'approval.json')
assert review['calibration_manifest_sha256']==CALHASH and review['host']==host
assert review['release_qualified'] is False
plan=read(PAIR/'plan.json')
sequence=[list(row) for row in m.paired.interleaved_sequence()]
assert len(sequence)==60 and plan['sequence']==sequence
assert (plan['reference'],plan['candidate'],plan['runner_class'],plan['procs'],plan['samples'],plan['order']) == (m.reference.REFERENCE,HEAD,m.CLASS,6,5,'interleaved')
assert plan['host']['allowed_cpus']==host['identity']['affinity']
predeclared=read(BASE/'qualification-plan.json')
assert predeclared['sequence']==sequence and predeclared['calibration_manifest_sha256']==CALHASH
assert predeclared['approval_sha256']==digest(BASE/'approval.json') and predeclared['regression_percent']==20
sources={'reference':source_manifest(m.reference.REFERENCE),'candidate':source_manifest(HEAD)}
assert plan['reference_source']==sources['reference'] and plan['candidate_source']==sources['candidate']
limits={'blocking-zone':CONTROL/'bench/limits.txt',**{name:CAL/(name+'-limits.txt') for name in m.CALIBRATED}}
limit_hashes={name:digest(path) for name,path in limits.items()}
flags=['go','run','./hack/benchgate','-samples','5','-procs','6','-strict-cpu','-threshold','20']
native=[]
gates=[]
role_ok={}
metric_counts={}
for role,scopes,reports,head in (
    ('reference',m.reference.SCOPES,m.reference.REPORTS,m.reference.REFERENCE),
    ('candidate',m.runner.SUITES,m.runner.REPORTS,HEAD)):
    directory=PAIR/role
    assert read(directory/'source.json')==sources[role]
    role_plan=read(directory/'plan.json')
    assert role_plan['head']==head and role_plan['control_head']==HEAD
    assert (role_plan['runner_class'],role_plan['procs'],role_plan['samples'],role_plan['order'])==(m.CLASS,6,5,'interleaved-v1')
    assert role_plan['go_version']=='go version go1.27.4 linux/amd64'
    assert role_plan['scope_limits_sha256']==limit_hashes and role_plan['limits_sha256']==limit_hashes['blocking-zone']
    assert role_plan['commands']=={scope:m.runner.commands()[scope] for scope in scopes}
    outcome=read(directory/'result.json')
    assert outcome['measurement_complete'] and outcome['release_qualified'] is False
    assert set(outcome['reports'])==set(reports)
    good=True
    for sample in range(1,6):
        for scope in scopes:
            label=f'{scope}-{sample}'
            receipt=read(directory/(label+'.json'))
            assert receipt['argv']==m.runner.commands()[scope]
            assert receipt['stop_reason'] is None and receipt['group_reaped']
            assert receipt['output_bytes']==(directory/(label+'.log')).stat().st_size
            good &= receipt['child_exit_code']==0
            native.append({'role':role,'label':label,**receipt})
    for report,report_scopes in reports.items():
        text=''
        for sample in range(1,6):
            for scope in report_scopes:
                label=f'{scope}-{sample}'
                text+=(directory/(label+'.log')).read_text()
                if read(directory/(label+'.json'))['child_exit_code']:
                    text+='\nFAIL: go test exited non-zero\n'
        path=directory/(report+'.txt')
        assert path.read_text()==text and digest(path)==outcome['reports'][report]
        metric_counts[role+'/'+report]=len(m.vectors(text))
        receipt=read(directory/(report+'-gate.json'))
        assert receipt['argv']==flags+['-limits',str(limits[report]),str(path)]
        assert receipt['stop_reason'] is None and receipt['group_reaped']
        assert receipt['output_bytes']==(directory/(report+'-gate.log')).stat().st_size
        good &= receipt['child_exit_code']==0
        gates.append({'role':role,'report':report,'exit_code':receipt['child_exit_code']})
    assert outcome['exit_code']==int(not good)
    verified,_=m.paired.verify_recording(directory,reports,scopes)
    assert verified==good
    role_ok[role]=good
assert len(native)==60 and len({item['pid'] for item in native})==60 and len(gates)==10
console=(BASE/'qualification-console.log').read_text().splitlines()
starts=[line.split()[1:] for line in console if line.startswith('START ')]
assert starts==[[role,f'{scope}-{sample}'] for role,scope,sample in sequence]
completions=[line.split()[1:] for line in console if line.startswith('COMPLETE ')]
lookup={(item['role'],item['label']):item['child_exit_code'] for item in native}
assert completions==[[role,f'{scope}-{sample}',str(lookup[role,f'{scope}-{sample}'])] for role,scope,sample in sequence]
result=read(PAIR/'result.json')
assert result['measurement_complete'] and result['release_qualified'] is False
assert result['reference_exit']==int(not role_ok['reference']) and result['candidate_exit']==int(not role_ok['candidate'])
assert result['comparison_eligible']==role_ok['reference']
comparisons={}
if result['comparison_eligible']:
    assert set(result['comparisons'])==set(m.reference.REPORTS)
    for report in m.reference.REPORTS:
        receipt=read(PAIR/(report+'-compare.json'))
        assert receipt['argv']==flags+['-limits',str(limits[report]),'-baseline',str(PAIR/'reference'/(report+'.txt')),str(PAIR/'candidate'/(report+'.txt'))]
        assert receipt['stop_reason'] is None and receipt['group_reaped']
        assert receipt['output_bytes']==(PAIR/(report+'-compare.log')).stat().st_size
        comparisons[report]=receipt['child_exit_code']
    assert result['comparisons']==comparisons
else:
    assert result['comparisons']=={} and result['comparison_refusal']=='historical reference measurements or gates failed'
passed=all(role_ok.values()) and result['comparison_eligible'] and len(comparisons)==3 and not any(comparisons.values())
assert result['exit_code']==int(not passed)
assert read(BASE/'qualification-execution-result.json')['exit_code']==result['exit_code']
files={str(path.relative_to(QUAL)):digest(path) for path in sorted(QUAL.rglob('*')) if path.is_file()}
(BASE/'qualification-evidence-manifest.json').write_text(json.dumps({'files':files,'release_qualified':False},indent=2)+'\n')
summary={'performance_pass':passed,'runtime_source':'5e19a32bfb4d62f8524218f0e1cec3df6806eaa0',
    'candidate_source':HEAD,'reference_source':m.reference.REFERENCE,'runner_class':m.CLASS,
    'calibration_manifest_sha256':CALHASH,'approval_sha256':digest(BASE/'approval.json'),
    'qualification_evidence_manifest_sha256':digest(BASE/'qualification-evidence-manifest.json'),
    'files_verified':len(files),'source_paths_verified':{role:len(paths) for role,paths in sources.items()},
    'native_commands':60,'native_failures':[item['role']+'/'+item['label'] for item in native if item['child_exit_code']],
    'minimum_disk_free_bytes':min(item['minimum_free_bytes'] for item in native),
    'absolute_gates':gates,'comparison_eligible':result['comparison_eligible'],'relative_checks':comparisons,
    'metric_counts':metric_counts,'exit_code':result['exit_code'],'release_qualified':False}
(BASE/'qualification-verification.json').write_text(json.dumps(summary,indent=2)+'\n')
print(json.dumps(summary,indent=2))
