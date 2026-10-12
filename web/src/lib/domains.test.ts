import { describe, expect, it } from 'vitest';
import { actorName, firstValueError, normaliseValue, refPath, scopeText, shortRef, skippedText, splitValues, uiListAction, valueError } from './domains';
import { enabledEntriesText } from './lists';

describe('allow and deny entry values', () => {
  it('validates exact names like the server and points wildcards to their type', () => {
    expect(valueError('exact', 'ads.example.com')).toBeNull();
    expect(valueError('exact', 'Ads.Example.COM.')).toBeNull();
    expect(valueError('exact', '_dmarc.example.com')).toBeNull();
    expect(valueError('exact', 'bad..example')).toMatch(/not a domain name/);
    expect(valueError('exact', '*.example.com')).toMatch(/use the type Wildcard/);
    expect(valueError('exact', '||example.com^')).toMatch(/use the type Wildcard/);
    expect(valueError('exact', '')).toBe('Required');
  });

  it('accepts the three wildcard spellings and stores them as *.x', () => {
    for (const v of ['*.example.com', '||example.com^', 'example.com']) {
      expect(valueError('wildcard', v)).toBeNull();
      expect(normaliseValue('wildcard', v)).toBe('*.example.com');
    }
    expect(valueError('wildcard', '*.ads.*.example')).toMatch(/only a leading/);
    expect(valueError('wildcard', '*.bad..example')).toMatch(/not a domain name/);
  });

  it('checks RE2 expressions with or without slashes', () => {
    expect(valueError('regex', '^ads?[0-9]*\\.')).toBeNull();
    expect(valueError('regex', '/^ads?[0-9]*\\./')).toBeNull();
    expect(normaliseValue('regex', '/^ads\\./')).toBe('^ads\\.');
    expect(valueError('regex', '(')).toMatch(/Not a valid expression/);
    expect(valueError('regex', '^(?=ads)')).toMatch(/RE2/);
    expect(valueError('regex', '(a)\\1')).toMatch(/RE2/);
  });

  it('splits one value per line, names also by spaces and commas', () => {
    expect(splitValues('exact', 'a.example, b.example\n c.example\n# note')).toEqual(['a.example', 'b.example', 'c.example']);
    expect(splitValues('regex', '^a b$\n^c,d$')).toEqual(['^a b$', '^c,d$']);
    expect(firstValueError('exact', ['ok.example', 'bad..example'])).toEqual({ value: 'bad..example', error: expect.stringMatching(/not a domain/) as string });
  });
});

describe('entry helpers', () => {
  it('formats refs, actors, scopes and the skipped policies', () => {
    expect(shortRef('default/kids')).toBe('kids');
    expect(shortRef('lab/kids')).toBe('lab/kids');
    expect(refPath('requests', 'default/x-1')).toBe('/requests/default/x-1');
    expect(actorName('User:operator')).toBe('operator');
    expect(actorName('system')).toBe('system');
    expect(scopeText({ everyone: false, groups: ['default/kids', 'lab/iot'], namespace: 'default' })).toBe('kids, lab/iot');
    expect(uiListAction('ui-deny-kids')).toBe('deny');
    expect(uiListAction('stevenblack')).toBeUndefined();
    expect(skippedText(['default/iot-strict'], 'allow')).toBe('Not applied for iot-strict: managed elsewhere, add the list to its allowListRefs there.');
    expect(enabledEntriesText({ entries: 3 })).toBe('3');
    expect(enabledEntriesText({ entries: 1200, excluded: 2 })).toBe('1,200 of 1,202 enabled');
  });
});
