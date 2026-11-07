import fs from 'node:fs';
import path from 'node:path';
import { expect, expectRendered, main, settle, test } from './fixtures';
import { adminApi, object, suffix } from './support/api';
import { choose } from './support/astryx';
import { REPO_DIR } from './support/kudurru';
import type { Policy } from '../src/api/types';

test('policy response controls save explicit overrides and affect queries', async ({ page }) => {
  const id = `response-${suffix()}`;
  const name = `${id}.example`;
  const client = '192.0.2.247';
  await page.goto('/policies');
  const api = await adminApi(page);
  await api.put(`/objects/clientgroups/default/${id}`, object('ClientGroup', id, { clients: [{ ip: client }] }));
  await api.put(`/objects/policies/default/${id}`, object('Policy', id, { clientGroupRefs: [id], priority: 700 }));
  const dig = () => api.post<{ answer: string[]; rcode: string; rule?: string; flags: { ad: boolean } }>('/dns/query', { name, type: 'A', client });
  try {
    await page.goto(`/policies/default/${id}`);
    await settle(page);
    const section = main(page).getByTestId('section-response');
    await section.getByRole('button', { name: 'Edit Response processing' }).click();
    await choose(section.getByRole('combobox', { name: 'DNS64 for these clients' }), 'Off');
    await choose(section.getByRole('combobox', { name: 'IPv6 answers for these clients' }), 'Keep IPv6');
    await section.getByRole('button', { name: 'Add rewrite' }).click();
    await section.getByRole('textbox', { name: 'Rewrite 1 name' }).fill(name);
    await section.getByRole('textbox', { name: 'Rewrite 1 targets 1' }).fill('192.0.2.89');
    const screenshots = path.join(REPO_DIR, '.context');
    fs.mkdirSync(screenshots, { recursive: true });
    await section.screenshot({ path: path.join(screenshots, 'response-policy-editor.png') });
    await section.getByRole('button', { name: 'Save Response processing' }).click();
    await expect(section.getByRole('button', { name: 'Save Response processing' })).toHaveCount(0);
    const saved = (await api.get<Policy>(`/objects/policies/default/${id}`)).body;
    expect(saved.spec.response).toEqual({ dns64Prefix: '', filterAAAA: false });
    expect(saved.spec.rewrites).toEqual([{ name, type: 'A', targets: ['192.0.2.89'] }]);
    await expect.poll(async () => (await dig()).answer.join('\n')).toContain('192.0.2.89');
    const answer = await dig();
    expect(answer.flags.ad).toBe(false);
    expect(answer.rule).toContain('policy_rewrite');
    await page.reload();
    await settle(page);
    await expect(section).toContainText('1 rewrite · DNS64 off · IPv6 kept');
    await expectRendered(page);
    await section.screenshot({ path: path.join(screenshots, 'response-policy-saved.png') });
  } finally {
    await api.del(`/objects/policies/default/${id}`);
    await api.del(`/objects/clientgroups/default/${id}`);
  }
});
