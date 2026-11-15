"""Build a human review of existing measurements; never change or accept bounds."""
import csv
import importlib.util
import json
from pathlib import Path
import re
import statistics

BASE = Path(__file__).resolve().parent
CAL = BASE/'calibration'
METHOD = BASE.parent/'cloud-benchmark-method'
spec = importlib.util.spec_from_file_location('dedicated', METHOD/'hack/bench-dedicated.py')
m = importlib.util.module_from_spec(spec)
spec.loader.exec_module(m)
verified = json.loads((BASE/'verification.json').read_text())
values = m.vectors((CAL/'blocking-zone.txt').read_text())
fixed = []
for line in (METHOD/'bench/limits.txt').read_text().splitlines():
    fields = line.split()
    if not fields or fields[0] != 'limit':
        continue
    _, pattern, unit, bound = fields
    for (name, metric_unit), vector in sorted(values.items()):
        if unit == metric_unit and re.search(pattern, name):
            median = statistics.median(vector)
            fixed.append({'name':name, 'unit':unit, 'values':vector, 'median':median,
                          'limit':float(bound), 'pass':median <= float(bound)})
(BASE/'fixed-product-metrics.json').write_text(json.dumps(fixed,indent=2)+'\n')
eligible = verified['eligible_for_limit_review']
lines = ['# Cloud-sandbox historical calibration: '+('eligible for review' if eligible else 'failed'), '',
    ('All 40 historical native commands and seven shape/product gates pass. The '
     'calibration is eligible for owner review of these exact vectors and proposed '
     'limits. Approval remains pending; no candidate timing has run.' if eligible else
     'The historical calibration is ineligible. No candidate timing is authorized '
     'and no proposed profile or approval template can be credited.'), '',
    'Performance remains release-blocking until actual limit review and passing '
    'final qualification. The original failed hosted D412 and failed shared '
    'homelab recording remain failed. No sample was replaced or bound relaxed.', '',
    '## Source and host', '',
    '- Method/source `3ff60b9f21f9d2abe114eed418b2179d6f28032d`, profile class '
    '`shared-cloud-sandbox-v1`. Runtime tree matches `5e19a32b` except retained '
    'benchmark tooling and documentation.',
    '- Original reference `0cc9565e9a878f8223fc1cee5201eb7eca8aacd2` for '
    'blocking/zone/cache/upstream; later `9cc2ae0423070fc514c94a25ab5b92e3816889e7` '
    'for UDP/DoT/QUIC/persistence.',
    '- This named eight-vCPU Vercel sandbox, Intel Xeon family6/model85/stepping7, '
    'KVM, Go1.27.4, CGO0, six benchmark processors, five fresh processes per scope. '
    'Physical host, pinning, SMT placement and tenant sharing are unavailable and '
    'explicitly accepted by the owner in the retained method decision.',
    '- The actual owner-reviewed proposal, approval evidence and guest identity '
    'are retained. Guest/cgroup/toolchain/storage/source guards run around every '
    'native process. Unrelated local builds/tests finished before measurement.',
    f"- Verified {verified['files_verified']} artifact hashes and source path counts "
    +', '.join(f'{key}: {count}' for key,count in verified['source_paths_verified'].items())+'.',
    f"- All {verified['native_commands']} native receipts are retained; minimum free "
    f"disk {verified['minimum_disk_free_bytes']:,} bytes. Gate results are in verification.json.", '',
    '## Exact review identity', '', 'Calibration manifest SHA256:', '', '```text',
    verified['manifest_sha256'], '```', '',
    'Accepting a host or this report generation does not accept the values. '
    'The separate review record must bind this exact digest. `release_qualified` '
    'remains false.', '', '## Fixed product requirements', '',
    '| Metric | Unit | Five values | Median | Fixed maximum | Result |',
    '| --- | --- | --- | ---: | ---: | --- |']
for item in fixed:
    lines.append('| `'+item['name']+'` | '+item['unit']+' | '+', '.join(f'{v:g}' for v in item['values'])+
                 f" | {item['median']:g} | {item['limit']:g} | "+('PASS' if item['pass'] else 'FAIL')+' |')
if eligible:
    provenance = {'runner_class':m.CLASS, 'cpu':'Intel Xeon family6/model85/stepping7 at2.50GHz; KVM, physical placement opaque and owner-accepted',
        'go_version':'go version go1.27.4 linux/amd64', 'procs':6, 'samples':5,
        'sources':json.loads((CAL/'plan.json').read_text())['sources'],
        'control_source':'3ff60b9f21f9d2abe114eed418b2179d6f28032d',
        'recording':'v020-cloud-shared-20261115', 'calibration_eligible':True,
        'calibration_manifest_sha256':verified['manifest_sha256'],
        'approval':{'decision':'pending'}, 'release_qualified':False,
        'formula':'upper median + max(20%, 25ns/op or64B/op noise); minima80% of median; zero loss/failure',
        'scopes':{}}
    comparisons = []
    movements = {'more_restrictive':0, 'less_restrictive':0, 'unchanged':0}
    lines += ['', '## Proposed hardware profiles', '',
        'All values below derive exclusively from the immutable historical '
        'recordings. Upper bounds are median plus max(20%, 25ns/op or 64B/op '
        'where applicable); other upper units retain 20%. Throughput and entry '
        'minima are 80% of median. Every loss/failure vector must be zero. The '
        'original repository profiles and fixed product rules are unchanged.']
    for scope in m.CALIBRATED:
        vectors = m.vectors((CAL/(scope+'.txt')).read_text())
        rules=[]
        for line in (CAL/(scope+'-limits.txt')).read_text().splitlines():
            fields=line.split()
            if fields and fields[0] in ('limit','minimum','zero'):
                rules.append(fields)
        legacy=[]
        for line in (METHOD/m.runner.LIMIT_FILES[scope]).read_text().splitlines():
            fields=line.split()
            if fields and fields[0] in ('limit','minimum','zero'):
                legacy.append(fields)
        provenance['scopes'][scope] = {'source':m.reference.REFERENCE if scope in m.reference.SCOPES else m.LATER,
                                      'metrics':[]}
        lines += ['', '### '+scope, '',
            '| Metric | Unit | Five values | Median | Rule | Proposed bound | Legacy bound |',
            '| --- | --- | --- | ---: | --- | ---: | ---: |']
        for (name,unit),vector in sorted(vectors.items()):
            matching=[rule for rule in rules if rule[2]==unit and re.fullmatch(rule[1],name)]
            assert len(matching)==1
            rule=matching[0]
            bound=0 if rule[0]=='zero' else float(rule[3])
            median=statistics.median(vector)
            expected=(0 if unit in ('failed','lost') else median*.8 if unit in ('q/s','MB/s','entries') else
                      median+max(median*.2,25 if unit=='ns/op' else 64 if unit=='B/op' else 0))
            assert abs(bound-expected) <= max(1e-9,abs(expected)*1e-10)
            previous=[row for row in legacy if row[2]==unit and re.fullmatch(row[1],name)]
            assert len(previous)==1 and previous[0][0]==rule[0]
            old_bound=0 if rule[0]=='zero' else float(previous[0][3])
            movement=('unchanged' if bound==old_bound else
                      'less_restrictive' if (bound>old_bound if rule[0]=='limit' else bound<old_bound) else
                      'more_restrictive')
            movements[movement]+=1
            metric={'name':name, 'unit':unit, 'values':vector, 'median':median,
                    'noise_floor':25 if unit=='ns/op' else 64 if unit=='B/op' else 0,
                    'rule':rule[0], rule[0]:bound, 'legacy_bound':old_bound, 'movement':movement}
            provenance['scopes'][scope]['metrics'].append(metric)
            comparisons.append({'scope':scope,'name':name,'unit':unit,
                **{f'sample_{i}':v for i,v in enumerate(vector,1)},
                'median':median,'rule':rule[0],'proposed_bound':bound,'legacy_bound':old_bound,'movement':movement})
            lines.append('| `'+name+'` | '+unit+' | '+', '.join(f'{v:g}' for v in vector)+
                         f' | {median:g} | {rule[0]} | {bound:g} | {old_bound:g} |')
    (BASE/'provenance.json').write_text(json.dumps(provenance,indent=2)+'\n')
    (BASE/'profile-comparison-summary.json').write_text(json.dumps(movements,indent=2)+'\n')
    with (BASE/'profile-comparison.csv').open('w',newline='') as output:
        writer=csv.DictWriter(output,fieldnames=list(comparisons[0]),lineterminator='\n')
        writer.writeheader()
        writer.writerows(comparisons)
    marker='## Proposed hardware profiles'
    index=lines.index(marker)
    lines.insert(index+1, '\nCompared with the legacy profiles: '+
        ', '.join(f'{count} {name.replace("_"," ")}' for name,count in movements.items())+
        '. Fixed product requirements do not change. Every individual old/new bound is shown below and in profile-comparison.csv.')
lines += ['', 'The raw calibration directory retains every native log/receipt, '
    'complete report, original-source manifest, exact command plan, shape rules '
    'and result. No native process is rerun by this verifier or report generator.', '']
(BASE/'REVIEW.md').write_text('\n'.join(lines))
print('Prepared REVIEW.md; approval remains pending, eligible='+str(eligible))
