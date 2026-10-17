import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { Heading } from '@astryxdesign/core/Heading';
import { MetadataList, MetadataListItem } from '@astryxdesign/core/MetadataList';
import { Section } from '@astryxdesign/core/Section';
import { HStack, VStack } from '@astryxdesign/core/Stack';
import { Text } from '@astryxdesign/core/Text';
import { useState } from 'react';
import { ApiError } from '../api/client';
import { useCan } from '../api/context';
import type { DHCPFailover, DHCPSetup, DHCPSetupScope, DHCPStatus } from '../api/types';
import { ErrorBanner } from './Feedback';
import { setupChanged, setupRequest, useSaveDHCPSetup } from './DHCPSetup';
import { SelectField } from './fields/SelectField';
import { CheckboxField, SwitchField, TextField } from './fields/TextField';
import { formatError } from './fields/formats';

const memberID = (v: string) => /^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/.test(v);
export const newDHCPFailover = (): DHCPFailover => ({ enabled: false, clusterID: '', nodeID: '', listenAddress: '', peers: [], bootstrap: false, caFile: '', certFile: '', keyFile: '' });
const firstVoter = (f: DHCPFailover) => (f.peers ?? []).map((p) => p.id).filter(Boolean).sort()[0];

export function failoverError(f: DHCPFailover, scopes: DHCPSetupScope[]): string | undefined {
  if (!f.enabled) return;
  if (!memberID(f.clusterID) || !memberID(f.nodeID)) return 'Use stable lowercase names for the cluster and this node.';
  if (!f.listenAddress || formatError('hostport', f.listenAddress)) return 'Enter the local peer listener as host:port.';
  const peers = f.peers ?? [];
  if (peers.length !== 3 || peers.some((p) => !memberID(p.id) || !p.address || formatError('hostport', p.address))) return 'Complete all three voter IDs and peer addresses.';
  if (new Set(peers.map((p) => p.id)).size !== 3 || new Set(peers.map((p) => p.address)).size !== 3) return 'Voter IDs and peer addresses must be unique.';
  if (!peers.some((p) => p.id === f.nodeID)) return 'This node ID must match one of the three voters.';
  if (f.bootstrap && f.nodeID !== firstVoter(f)) return 'Only the alphabetically first voter may bootstrap a new cluster.';
  if ([f.caFile, f.certFile, f.keyFile].some(Boolean) && ![f.caFile, f.certFile, f.keyFile].every(Boolean)) return 'Provide all three TLS file paths, or use the shared cluster encryption key.';
  if (peers.some((p) => {
    if (!p.apiURL) return false;
    try { const url = new URL(p.apiURL); return !['http:', 'https:'].includes(url.protocol) || !!url.username || !!url.password || !!url.search || !!url.hash; }
    catch { return true; }
  })) return 'Dashboard addresses must be HTTP or HTTPS URLs without credentials, query parameters or fragments.';
  for (const scope of scopes) {
    const servers = scope.failoverServers ?? [];
    if (servers.length !== 3 || peers.some((p) => !servers.some((s) => s.nodeID === p.id && s.address && !formatError('ipv4', s.address))) || new Set(servers.map((s) => s.address)).size !== 3) return `Enter a distinct IPv4 server address for every voter in scope ${scope.name}.`;
    if (servers.find((s) => s.nodeID === f.nodeID)?.address !== scope.serverIP) return `The local failover server address in ${scope.name} must match ${scope.serverIP}.`;
  }
}

function mapServers(scopes: DHCPSetupScope[], f: DHCPFailover): DHCPSetupScope[] {
  return scopes.map((scope) => ({ ...scope, failoverServers: (f.peers ?? []).map((peer) => ({ nodeID: peer.id,
    address: scope.failoverServers?.find((s) => s.nodeID === peer.id)?.address || (peer.id === f.nodeID ? scope.serverIP : '') })) }));
}

export function DHCPFailoverStatus({ value }: { value?: DHCPStatus['failover'] }) {
  if (!value?.enabled) return null;
  const leader = value.role.toLowerCase() === 'leader';
  return <Section padding={0} variant="transparent"><VStack gap={3}>
    <Heading level={2}>Automatic failover status</Heading>
    <Banner status={value.error || (!value.leaderID && !value.ready) ? 'warning' : 'info'}
      title={value.ready ? 'Leader has quorum' : leader ? 'Quorum is unavailable' : value.leaderID ? 'Replica follows the leader' : 'Waiting for a quorum leader'}
      description={value.error || (leader && !value.ready ? 'DHCP requests are dropped until a majority is available. DNS continues serving.' : !leader ? 'This node keeps replicated lease state. DHCP replies come from the leader; management writes route there automatically.' : 'New offers, bindings and management changes require durable agreement from at least two voters.')} />
    <MetadataList columns="multi">
      <MetadataListItem label="Cluster">{value.clusterID}</MetadataListItem>
      <MetadataListItem label="This voter">{value.nodeID} · {value.role}</MetadataListItem>
      <MetadataListItem label="Leader">{value.leaderID || 'Not elected'}</MetadataListItem>
      <MetadataListItem label="Applied / committed index">{value.appliedIndex} / {value.commitIndex}</MetadataListItem>
    </MetadataList>
  </VStack></Section>;
}

// Existing page section: edits are reviewed and saved inline, without nesting
// a second large dialog inside the DHCP scope wizard.
export function DHCPFailoverEditor({ setup }: { setup: DHCPSetup }) {
  const can = useCan();
  const save = useSaveDHCPSetup();
  const [base, setBase] = useState(setup);
  const [draft, setDraft] = useState(() => structuredClone(setup.failover ?? newDHCPFailover()));
  const [scopes, setScopes] = useState(() => structuredClone(setup.scopes));
  const [editing, setEditing] = useState(false);
  const [submitted, setSubmitted] = useState(false);
  const [credentials, setCredentials] = useState(() => setup.failover?.caFile || setup.failover?.certFile || setup.failover?.keyFile ? 'files' : 'cluster');
  const initialized = setup.failoverInitialized;
  const writable = setup.available && setup.managedBy === 'api' && !setup.environmentLocked && can('dhcp', 'write');
  const disabled = !writable || save.isPending;
  const stale = setupChanged(setup, base) || (save.error instanceof ApiError && save.error.status === 412);
  const validation = draft.enabled && credentials === 'files' && ![draft.caFile, draft.certFile, draft.keyFile].every((path) => path.trim())
    ? 'Provide the CA, certificate and private key file paths for this node.' : failoverError(draft, scopes);
  const peers = Array.from({ length: 3 }, (_, i) => draft.peers?.[i] ?? { id: '', address: '', apiURL: '' });
  const change = <K extends keyof DHCPFailover>(key: K, value: DHCPFailover[K]) => setDraft((v) => ({ ...v, [key]: value }));
  const reset = () => { setBase(setup); setDraft(structuredClone(setup.failover)); setScopes(structuredClone(setup.scopes)); setSubmitted(false); setCredentials(setup.failover.caFile ? 'files' : 'cluster'); save.reset(); };
  const updatePeer = (i: number, key: 'id' | 'address' | 'apiURL', value: string) => {
    const next = peers.map((peer, index) => index === i ? { ...peer, [key]: value } : peer);
    const f = { ...draft, peers: next };
    setDraft(f);
    setScopes((current) => mapServers(current.map((s) => ({ ...s, failoverServers: s.failoverServers?.map((server) => server.nodeID === peers[i]?.id ? { ...server, nodeID: next[i]?.id ?? '' } : server) })), f));
  };
  return <Section padding={0} variant="transparent"><VStack gap={4}>
    <HStack gap={2} hAlign="between" wrap="wrap" vAlign="center"><Heading level={2}>Automatic failover</Heading><Button label={editing ? 'Hide failover editor' : 'Configure failover'} onClick={() => { if (!editing) reset(); setEditing(!editing); }} /></HStack>
    <Text type="supporting">{setup.failover.enabled ? `Three voters · cluster ${setup.failover.clusterID} · this node ${setup.failover.nodeID}.` : 'Use three Kudurru nodes to retain DHCP ownership when one node is unavailable.'}</Text>
    {editing ? <VStack as="form" gap={4} aria-label="Automatic failover setup" onSubmit={(e) => {
      e.preventDefault(); setSubmitted(true);
      if (writable && !stale && !validation) save.mutate({ ...setupRequest(base), failover: draft, scopes }, { onSuccess: (next) => { setBase(next); setDraft(structuredClone(next.failover)); setScopes(structuredClone(next.scopes)); setSubmitted(false); } });
    }}>
      {!writable ? <Text type="supporting">{!can('dhcp', 'write') ? 'Changing failover requires dhcp.write permission.' : setup.environmentLocked ? 'DHCP environment variables control this node.' : 'Adopt this setup in the dashboard before editing failover.'}</Text> : null}
      {initialized ? <Banner status="info" title="Cluster identity is established" description="Voters, shared scope topology and server addresses are fixed. Use each running scope’s network options and reservations editors for replicated changes. Disable DHCP above to stop this voter safely." /> : null}
      {stale ? <Banner status="warning" title="The saved setup changed" description="Your failover draft is retained. Reload saved failover settings before submitting another change."
        endContent={<Button label="Reload failover settings" onClick={reset} isDisabled={save.isPending || !setupChanged(setup, base)} />} /> : null}
      <SwitchField label="Automatic failover" value={draft.enabled} isDisabled={disabled || initialized} onChange={(enabled) => {
        const next = { ...draft, enabled, peers: draft.peers?.length ? draft.peers : peers };
        setDraft(next); if (enabled) setScopes((current) => mapServers(current, next));
      }} help="A majority of two voters must durably agree before an offer or acknowledgment is sent." />
      {draft.enabled ? <>
        <Heading level={3}>Cluster and voting nodes</Heading>
        <HStack gap={4} wrap="wrap">
          <TextField label="Failover cluster ID" value={draft.clusterID} onChange={(v) => change('clusterID', v)} isDisabled={disabled || initialized} placeholder="home-dhcp" help="The same stable ID on all three nodes." />
          <TextField label="This node ID" value={draft.nodeID} onChange={(v) => { const next = { ...draft, nodeID: v }; setDraft(next); setScopes((current) => mapServers(current, next)); }} isDisabled={disabled || initialized} placeholder="stone-1" help="Must match one voter and this node’s TLS certificate identity." />
          <TextField label="Peer listen address" value={draft.listenAddress} onChange={(v) => change('listenAddress', v)} isDisabled={disabled} placeholder="0.0.0.0:7447" help="TCP listener on this node. Peers must be able to reach its advertised address." />
        </HStack>
        {peers.map((peer, i) => <HStack key={i} gap={4} wrap="wrap">
          <TextField label={`Voter ${i + 1} ID`} value={peer.id} onChange={(v) => updatePeer(i, 'id', v)} isDisabled={disabled || initialized} placeholder={`stone-${i + 1}`} />
          <TextField label={`Voter ${i + 1} peer address`} value={peer.address} onChange={(v) => updatePeer(i, 'address', v)} isDisabled={disabled || initialized} placeholder={`192.0.2.${i + 2}:7447`} help="Reachable host:port for authenticated replication." />
          <TextField label={`Voter ${i + 1} dashboard URL`} value={peer.apiURL ?? ''} onChange={(v) => updatePeer(i, 'apiURL', v)} isDisabled={disabled} placeholder="https://dns.home.arpa" help="Optional dashboard link; lease replication uses the peer address." />
        </HStack>)}
        <Heading level={3}>Server addresses by scope</Heading>
        <Text type="supporting">Every voter needs its own fixed IPv4 server address. All three are excluded from allocation. Configure relays for all three; direct clients reach a replacement leader during rebinding.</Text>
        {scopes.map((scope) => <VStack key={scope.name} gap={2}><Heading level={4}>{scope.name}</Heading><HStack gap={4} wrap="wrap">
          {peers.map((peer, i) => <TextField key={i} label={`${scope.name}: ${peer.id || `voter ${i + 1}`} server address`}
            value={scope.failoverServers?.find((s) => s.nodeID === peer.id)?.address ?? ''} isDisabled={disabled || initialized} placeholder={peer.id === draft.nodeID ? scope.serverIP : '192.0.2.3'}
            onChange={(address) => setScopes((current) => mapServers(current, { ...draft, peers }).map((s) => s.name === scope.name ? { ...s, failoverServers: s.failoverServers?.map((server) => server.nodeID === peer.id ? { ...server, address } : server) } : s))}
            help={peer.id === draft.nodeID ? `Must equal this scope’s server address: ${scope.serverIP}.` : 'Address assigned to that voter for this client subnet.'} />)}
        </HStack></VStack>)}
        <Heading level={3}>Authenticated transport</Heading>
        <SelectField label="Peer credentials" value={credentials} isDisabled={disabled} onChange={(v) => { setCredentials(v); if (v === 'cluster') setDraft((f) => ({ ...f, caFile: '', certFile: '', keyFile: '' })); }}
          options={[{ value: 'cluster', label: 'Existing cluster encryption key', help: 'All three hosts need the same existing cluster encryption key. The key is never entered in the browser.' }, { value: 'files', label: 'Certificate files on this node', help: 'Certificates need client and server authentication, CN matching the node ID, and a SAN matching the advertised peer host.' }]} />
        {credentials === 'files' ? <HStack gap={4} wrap="wrap">
          <TextField label="Peer CA file" value={draft.caFile} onChange={(v) => change('caFile', v)} isDisabled={disabled} placeholder="/etc/kudurru/dhcp-ca.pem" />
          <TextField label="Peer certificate file" value={draft.certFile} onChange={(v) => change('certFile', v)} isDisabled={disabled} placeholder="/etc/kudurru/dhcp-node.pem" />
          <TextField label="Peer private key file" value={draft.keyFile} onChange={(v) => change('keyFile', v)} isDisabled={disabled} placeholder="/etc/kudurru/dhcp-node.key" help="Only the file path is saved. Provision secret files on the host." />
        </HStack> : null}
        <CheckboxField label="Bootstrap this new cluster" value={draft.bootstrap} onChange={(v) => change('bootstrap', v)} isDisabled={disabled || initialized || !draft.nodeID || draft.nodeID !== firstVoter(draft)}
          help={`Enable only on ${firstVoter(draft) || 'the alphabetically first voter'} for the first startup. Existing live leases on that node seed the cluster; joining voters must have no independent ownership.`} />
        <Text type="supporting">Save matching cluster, voters and shared scope settings on all three nodes, restart them, then inspect quorum status. Fresh bootstrap requires explicit authorization on the first voter. Existing Raft state is reused on restart.</Text>
      </> : null}
      {submitted && validation ? <Banner status="error" title="Check failover settings" description={validation} /> : null}
      {save.error ? <ErrorBanner error={save.error} /> : null}
      <HStack gap={2}><Button label="Save failover for restart" type="submit" variant="primary" isDisabled={disabled || stale} isLoading={save.isPending} /><Button label="Reset failover changes" onClick={reset} isDisabled={save.isPending} /></HStack>
    </VStack> : null}
  </VStack></Section>;
}
