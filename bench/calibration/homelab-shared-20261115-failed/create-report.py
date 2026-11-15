"""Summarize retained evidence without executing measurements or changing limits."""
import importlib.util
import json
from pathlib import Path
import re
import statistics

base = Path(__file__).resolve().parent
cal = base / 'guest-evidence/calibration'
method = base.parent / 'dedicated-benchmark-method'
spec = importlib.util.spec_from_file_location('dedicated', method/'hack/bench-dedicated.py')
m = importlib.util.module_from_spec(spec)
spec.loader.exec_module(m)
verified = json.loads((base/'verification.json').read_text())
values = m.vectors((cal/'blocking-zone.txt').read_text())
fixed = []
for line in (method/'bench/limits.txt').read_text().splitlines():
    fields = line.split()
    if not fields or fields[0] != 'limit':
        continue
    _, pattern, unit, bound = fields
    for (name, metric_unit), vector in sorted(values.items()):
        if unit == metric_unit and re.search(pattern, name):
            median = statistics.median(vector)
            fixed.append({'name': name, 'unit': unit, 'values': vector,
                          'median': median, 'limit': float(bound), 'pass': median <= float(bound)})
(base/'fixed-product-metrics.json').write_text(json.dumps(fixed, indent=2)+'\n')
failed = [item for item in fixed if not item['pass']]
assert {item['name'] for item in failed} == {'BenchmarkLookup100k/hit', 'BenchmarkLookup100k/cname', 'BenchmarkLookup100k/wildcard'}
lines = ['# Shared-host historical calibration: failed', '',
    'One predeclared recording ran November 15, 2026, from 20:08:00 to 20:26:31 UTC. '
    'The owner accepted the pinned VM with disclosed physical CPU sharing before timing. '
    'All 40 native commands completed successfully, but the blocking/zone product gate failed. '
    'The calibration is ineligible; no proposed limit files, approval template or candidate '
    'measurement were produced. Performance remains release-blocking.', '',
    '| Failed historical metric | Five p99 values, ns | Median, ns | Fixed maximum, ns |',
    '| --- | --- | ---: | ---: |']
for item in failed:
    lines.append('| `'+item['name']+'` | '+', '.join(f'{value:g}' for value in item['values'])+
                 f" | {item['median']:g} | {item['limit']:g} |")
lines += ['', 'The other fixed product metrics pass. The cache, upstream, UDP, DoT, QUIC '
    'and persistence shape gates also pass. Those six gates validate shape, complete metric '
    'inventory and native outcomes; they do not claim that legacy hardware ceilings passed. '
    'The hardware profile was withheld because the required fixed product gate failed.', '',
    '## Provenance and verification', '',
    '- VM 20027, `kudurru-dev-test`, physical host `hm-pve02` (`hm-e-pve02.ssh`), '
    'eight vCPUs pinned to logical CPUs `0,2,4,6,8,10,12,14`, 16 GiB RAM and local NVMe. '
    'Other guests retain unrestricted affinity; no physical exclusivity is claimed.',
    '- Ubuntu 24.04, Go 1.27.4, CGO disabled, six benchmark processors; five fresh '
    'processes per scope. Original corpora, commands and durations were retained.',
    '- Original historical source: `0cc9565e9a878f8223fc1cee5201eb7eca8aacd2`; '
    'later socket/persistence source: `9cc2ae0423070fc514c94a25ab5b92e3816889e7`.',
    '- Collector and amended method: `fef3a1fde4cf8b847ba833d9e1f0710e7ec3f998`.',
    '- Stable execution cgroup: `/system.slice/kudurru-benchmark.service`. '
    'Service finished; no benchmark remains active.',
    f"- All {verified['files_verified']} manifest files verify; source manifests match "
    '406 original, 4,636 later and 5,348 control paths independently read from Git archives. '
    'All 40 receipts match the declared commands and have distinct process IDs, no resource '
    'stop and successful process-group cleanup.',
    f"- Minimum native-command free disk: {verified['minimum_disk_free_bytes']:,} bytes. "
    'Physical host and VM configuration match before and after. Guest/hypervisor contention '
    'counters are retained; these observations do not identify a cause of the latency misses.',
    '- Fixed product rules, original limits, baseline, samples and regression thresholds '
    'were not changed. No sample was replaced or timing run repeated.', '',
    'Calibration manifest SHA256:', '', '```text', verified['manifest_sha256'], '```', '',
    'The complete raw recording is under `guest-evidence/calibration/`, including the '
    'original reports, logs, receipts, rules, source manifests, plan and failed result. '
    '`verification.json` records the independent checks, and `fixed-product-metrics.json` '
    'contains every fixed-product vector and evaluated median. Top-level host decisions, '
    'execution receipts, service properties and before/after counters accompany it.', '',
    'Another attempt requires a diagnosed fix or a separately reviewed method change. '
    'The current recording cannot establish an accepted calibration or authorize candidate timing.', '']
(base/'REPORT.md').write_text('\n'.join(lines))
