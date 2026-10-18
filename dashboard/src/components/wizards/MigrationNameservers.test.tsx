import { fireEvent, screen } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { useState } from 'react';
import { describe, expect, it } from 'vitest';
import { server } from '../../test/setup';
import { renderWithApp } from '../../test/utils';
import { initialNameservers, MigrationNameservers, migrationNameserverError, nameserverRequest } from './MigrationNameservers';

function Editor() {
  const [value, setValue] = useState(initialNameservers('192.0.2.20'));
  return <MigrationNameservers value={value} onChange={setValue} />;
}

describe('Migration nameserver suggestions', () => {
  it('uses the navigated hostname and keeps IP literals out of NS names', () => {
    expect(nameserverRequest(initialNameservers('DNS.KUDURRU.TEST.'))).toEqual({ nameservers: ['dns.kudurru.test'], nameserverAddresses: [] });
    for (const host of ['localhost', 'foo.localhost', '', 'bad_name.test']) expect(initialNameservers(host).names).toEqual(['']);
    for (const [host, ip] of [['192.0.2.53', '192.0.2.53'], ['[2001:db8::53]', '2001:db8::53']]) {
      const value = initialNameservers(host);
      expect(nameserverRequest(value)).toEqual({ nameservers: [], nameserverAddresses: [ip] });
      expect(migrationNameserverError(value)).toBeNull();
    }
  });

  it('deduplicates VIPs from multiple nodes and only replaces edits on request', async () => {
    server.use(http.get('/api/v1/edge', () => HttpResponse.json({ vips: [
      { address: '192.0.2.53/24', state: 'MASTER' }, { address: '192.0.2.53/24', state: 'BACKUP' },
      { address: '2001:db8::53/64', state: 'MASTER' }, { address: '' },
    ] })));
    renderWithApp(<Editor />);
    fireEvent.change(screen.getByPlaceholderText('192.0.2.53'), { target: { value: '192.0.2.99' } });
    const detected = await screen.findByRole('button', { name: 'Use detected VIPs' });
    expect(screen.getByPlaceholderText('192.0.2.53')).toHaveValue('192.0.2.99');
    fireEvent.click(detected);
    expect(screen.getAllByPlaceholderText('192.0.2.53').map((e) => (e as HTMLInputElement).value)).toEqual(['192.0.2.53', '2001:db8::53']);
  });
});
