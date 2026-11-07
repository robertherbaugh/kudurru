import { fireEvent, render, screen } from '@testing-library/react';
import { useState } from 'react';
import { describe, expect, it } from 'vitest';
import type { PolicySpec } from '../api/types';
import { getSelector, pick } from '../test/controls';
import { ResponsePolicyFields, responsePolicyError, responsePolicySummary } from './ResponsePolicyFields';

describe('response policy editor', () => {
  it('preserves explicit false and empty overrides and edits typed rewrites', () => {
    let saved: PolicySpec = {};
    function Editor() {
      const [spec, set] = useState<PolicySpec>({});
      saved = spec;
      return <ResponsePolicyFields spec={spec} onChange={set} namespace="default" />;
    }
    render(<Editor />);
    pick(getSelector('DNS64 for these clients'), 'Off');
    pick(getSelector('IPv6 answers for these clients'), 'Keep IPv6');
    expect(saved.response).toEqual({ dns64Prefix: '', filterAAAA: false });
    expect(responsePolicySummary(saved)).toContain('DNS64 off · IPv6 kept');
    fireEvent.click(screen.getByRole('button', { name: 'Add rewrite' }));
    fireEvent.change(screen.getByRole('textbox', { name: 'Rewrite 1 name' }), { target: { value: '*.example' } });
    fireEvent.change(screen.getByRole('textbox', { name: 'Rewrite 1 targets 1' }), { target: { value: '192.0.2.8' } });
    expect(responsePolicyError(saved)).toBeNull();
    expect(saved.rewrites).toEqual([{ name: '*.example', type: 'A', targets: ['192.0.2.8'] }]);
    pick(getSelector('DNS64 for these clients'), 'Inherit');
    pick(getSelector('IPv6 answers for these clients'), 'Inherit');
    expect(JSON.stringify(saved.response)).toBe('{}');
    fireEvent.click(screen.getByRole('button', { name: 'Remove rewrite 1' }));
    expect(saved.rewrites).toEqual([]);
  });

  it('validates malformed prefixes, families, empty rows and CNAME cardinality', () => {
    expect(responsePolicyError({ response: { dns64Prefix: '2001:db8::/72' } })).toContain('/96');
    expect(responsePolicyError({ rewrites: [{ name: 'example', type: 'AAAA', targets: ['192.0.2.1'] }] })).not.toBeNull();
    expect(responsePolicyError({ rewrites: [{ name: '', type: 'A', targets: [''] }] })).not.toBeNull();
    expect(responsePolicyError({ rewrites: [{ name: 'example', type: 'CNAME', targets: ['a.example', 'b.example'] }] })).not.toBeNull();
    expect(responsePolicyError({ rewrites: [{ name: 'example', cname: 'other.example.' }] })).toBeNull();
  });
});
