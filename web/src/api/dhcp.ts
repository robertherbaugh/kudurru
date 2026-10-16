import type { ApiClient } from './client';
import type { DHCPConfig, DHCPExport, DHCPImportResult, DHCPInterfaces, DHCPLeases, DHCPPoolOptions, DHCPReservation, DHCPSetup, DHCPSetupRequest, DHCPStatus } from './types';

const base = '/node/dhcp/v4';
const scopePath = (scope: string) => `${base}/scopes/${encodeURIComponent(scope)}`;

export function dhcpEndpoints(c: ApiClient) {
  return {
    status: () => c.get<DHCPStatus>(`${base}/status`),
    setup: () => c.get<DHCPSetup>(`${base}/setup`),
    saveSetup: (setup: DHCPSetupRequest, etag: string) => c.put<DHCPSetup>(`${base}/setup`, setup, { 'If-Match': etag }),
    interfaces: () => c.get<DHCPInterfaces>(`${base}/interfaces`),
    config: (scope: string) => c.get<DHCPConfig>(`${scopePath(scope)}/config`),
    update: (scope: string, options: DHCPPoolOptions, etag: string) =>
      c.put<DHCPConfig>(`${scopePath(scope)}/config`, options, { 'If-Match': etag }),
    leases: (scope: string, query: { q?: string; state?: string; continue?: string; limit?: number }, signal?: AbortSignal) =>
      c.get<DHCPLeases>(`${scopePath(scope)}/leases`, query, signal),
    reserve: (scope: string, reservation: DHCPReservation, etag: string) =>
      c.put<DHCPConfig>(`${scopePath(scope)}/reservations/${encodeURIComponent(reservation.mac)}`,
        { ip: reservation.ip, hostname: reservation.hostname }, { 'If-Match': etag }),
    removeReservation: (scope: string, mac: string, etag: string) =>
      c.request<DHCPConfig>('DELETE', `${scopePath(scope)}/reservations/${encodeURIComponent(mac)}`, { headers: { 'If-Match': etag } }),
    export: (scope: string) => c.get<DHCPExport>(`${scopePath(scope)}/export`),
    import: (scope: string, format: 'native' | 'dnsmasq', data: string) =>
      c.post<DHCPImportResult>(`${scopePath(scope)}/import`, { format, data }),
  };
}
