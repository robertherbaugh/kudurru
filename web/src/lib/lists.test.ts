import { describe, expect, it } from 'vitest';
import type { BlockList } from '../api/types';
import { sourceNameError, suggestName } from '../components/lists/SourceDialog';
import { entriesProblem, guessFormat, parseEntries, sourceName, sourcesSummary, sourceStatus, withSourceEnabled } from './lists';
import { toYAML } from './yaml';

const list = (sources: BlockList['spec']['sources'], status?: BlockList['status']): BlockList => ({
  apiVersion: 'kudurru.io/v1alpha1',
  kind: 'BlockList',
  metadata: { name: 'l', namespace: 'default' },
  spec: { sources },
  ...(status ? { status } : {}),
});

describe('list sources', () => {
  it('names sources like blocking.SourceName', () => {
    expect(sourceName(0, { name: 'main', url: 'https://x/y' })).toBe('main');
    expect(sourceName(0, { url: 'https://x/y' })).toBe('https://x/y');
    expect(sourceName(1, { configMapRef: { name: 'cm' } })).toBe('configmap/cm');
    expect(sourceName(2, { inline: ['a'] })).toBe('inline-2');
  });

  it('summarises enabled sources', () => {
    expect(sourcesSummary([{ url: 'https://a/' }])).toBe('1 source');
    expect(sourcesSummary([{ url: 'https://a/' }, { inline: ['x'] }])).toBe('2 sources');
    expect(sourcesSummary([{ url: 'https://a/' }, { inline: ['x'], enabled: false }, { url: 'https://b/' }, { url: 'https://c/' }])).toBe('3 of 4 enabled');
  });

  it('sets enabled: false and omits it again when enabled', () => {
    const l = list([{ name: 'a', url: 'https://a/' }, { name: 'b', inline: ['x'] }]);
    const off = withSourceEnabled(l, 1, false);
    expect(off.spec.sources[1]).toEqual({ name: 'b', inline: ['x'], enabled: false });
    expect(l.spec.sources[1]).not.toHaveProperty('enabled');
    expect(withSourceEnabled(off, 1, true).spec.sources[1]).toEqual({ name: 'b', inline: ['x'] });
  });

  it('finds the status of a source by its name', () => {
    const l = list([{ url: 'https://a/' }], { sources: [{ name: 'https://a/', entries: 4 }] });
    expect(sourceStatus(l, 0)?.entries).toBe(4);
  });

  it.each([
    ['https://raw.githubusercontent.com/StevenBlack/hosts/master/hosts', 'hosts'],
    ['https://small.oisd.nl/', 'adblock'],
    ['https://raw.githubusercontent.com/hagezi/dns-blocklists/main/adblock/pro.txt', 'adblock'],
    ['https://raw.githubusercontent.com/hagezi/dns-blocklists/main/domains/pro.txt', 'domains'],
    ['https://adguardteam.github.io/AdGuardSDNSFilter/Filters/filter.txt', 'adblock'],
    ['https://example.com/hosts.txt', 'hosts'],
    ['https://example.com/list', 'auto'],
  ])('guesses the format of %s', (url, want) => expect(guessFormat(url)).toBe(want));

  it('reads pasted entries and finds invalid ones by format', () => {
    expect(parseEntries('# comment\n\n  a.example \r\n! adblock comment\n||b.example^')).toEqual(['a.example', '||b.example^']);
    expect(entriesProblem(['a.example', 'bad..x', 'worse..x'])).toMatchObject({ first: 'bad..x', count: 2 });
    expect(entriesProblem(['^ads?\\.', '('], 'regex')).toMatchObject({ first: '(', count: 1 });
    expect(entriesProblem(['0.0.0.0 ads.example'], 'hosts')).toBeNull();
  });

  it('suggests and checks source names', () => {
    expect(suggestName('https://raw.githubusercontent.com/StevenBlack/hosts/master/hosts', [])).toBe('stevenblack-hosts');
    expect(suggestName('https://small.oisd.nl/', ['small'])).toBe('small-2');
    expect(sourceNameError('', [])).toMatch(/Required/);
    expect(sourceNameError('a b', [])).toMatch(/Letters/);
    expect(sourceNameError('main', ['main'])).toMatch(/Another source/);
    expect(sourceNameError('main', [])).toBeNull();
  });
});

describe('toYAML', () => {
  it('prints objects, lists and quoted scalars', () => {
    expect(
      toYAML({
        kind: 'BlockList',
        metadata: { name: 'x', annotations: { 'kudurru.io/enabled': 'false' } },
        spec: { sources: [{ name: 'a', inline: ['||ads.example^', '*.x.example', 'plain.example'], enabled: false }, { url: 'https://h/x' }], empty: [] },
        count: 3,
        y: 'yes',
      }),
    ).toBe(
      [
        'kind: BlockList',
        'metadata:',
        '  name: x',
        '  annotations:',
        '    kudurru.io/enabled: "false"',
        'spec:',
        '  sources:',
        '  - name: a',
        '    inline:',
        '    - "||ads.example^"',
        '    - "*.x.example"',
        '    - plain.example',
        '    enabled: false',
        '  - url: https://h/x',
        '  empty: []',
        'count: 3',
        '"y": "yes"',
        '',
      ].join('\n'),
    );
  });
});
