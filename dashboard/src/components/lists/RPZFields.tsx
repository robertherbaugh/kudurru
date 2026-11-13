// The transfer settings of an RPZ feed source (api/v1alpha1 RPZSource), shared
// by the Add source dialog and the new-list wizard: the policy zone, its
// primaries in order, TCP or TLS and an optional TSIG key name (never a
// secret). Each node transfers its own copy; docs/deploy/rpz.md.
import { Grid } from '@astryxdesign/core/Grid';
import { Text } from '@astryxdesign/core/Text';
import type { RPZDraft } from '../../lib/rpz';
import { rpzPrimariesError, rpzZoneError } from '../../lib/rpz';
import { primaryEndpointError, tsigKeyNameError } from '../../lib/zones';
import { EnumSelect } from '../fields/EnumSelect';
import { StringListField } from '../fields/StringListField';
import { TextField } from '../fields/TextField';

const PROTOCOLS = [
  { value: 'tcp', label: 'TCP', help: 'AXFR/IXFR over TCP, without encryption.' },
  { value: 'tls', label: 'TLS', help: "Encrypted transfers; the primary's certificate must verify for the host name you give." },
];

export interface RPZFieldsProps {
  value: RPZDraft;
  onChange: (patch: Partial<RPZDraft>) => void;
  /** Show the errors of the required fields (after a first submit). */
  showErrors?: boolean | undefined;
}

export function RPZFields({ value, onChange, showErrors }: RPZFieldsProps) {
  const zoneError = showErrors ? (rpzZoneError(value.zoneName) ?? undefined) : undefined;
  return (
    <>
      <TextField
        label="Policy zone"
        mono
        placeholder="rpz.vendor.example."
        value={value.zoneName}
        onChange={(v) => onChange({ zoneName: v })}
        help="The response policy zone the vendor publishes; its records become blocking rules."
        error={zoneError}
      />
      <StringListField
        label="Primary servers"
        description="Host names or IP addresses with an optional port (default 53); [2001:db8::53]:5353 for IPv6 with a port. Tried in order, 1 to 32."
        value={value.primaries}
        onChange={(v) => onChange({ primaries: v })}
        placeholder="192.0.2.53"
        itemError={primaryEndpointError}
        addLabel="Add primary server"
        error={showErrors ? (rpzPrimariesError(value.primaries) ?? undefined) : undefined}
      />
      <Grid columns={{ minWidth: 220, repeat: 'fit' }} gap={3} align="start">
        <EnumSelect label="Transfer transport" value={value.transferProtocol} options={PROTOCOLS} onChange={(v) => onChange({ transferProtocol: v === 'tls' ? 'tls' : 'tcp' })} />
        <TextField
          label="TSIG key name"
          isOptional
          mono
          placeholder="vendor-xfr."
          value={value.tsigKeyRef}
          onChange={(v) => onChange({ tsigKeyRef: v })}
          help="A stored or file TSIG key, by name only. Empty transfers unsigned."
          error={tsigKeyNameError(value.tsigKeyRef) ?? undefined}
        />
      </Grid>
      <Text type="supporting">
        Every node transfers its own copy and follows the zone's SOA timers. Until a node holds a copy, and after it expires, that node answers SERVFAIL to clients whose policy uses the list.
      </Text>
    </>
  );
}
