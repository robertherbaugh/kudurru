// Import a BIND master file as a new zone: POST /zones/{zone}/import with
// ?origin= creates the DNSZone from the file (the SOA and NS of the file
// become the zone's) and writes its records into API-managed record sets.
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { FileInput } from '@astryxdesign/core/FileInput';
import { Text } from '@astryxdesign/core/Text';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';
import { qk, useApi } from '../api/context';
import type { ZoneImportResult } from '../api/types';
import { nameError } from '../lib/objectSchemas';
import { useNavigate } from '../lib/router';
import { hostError } from '../lib/rr';
import { errorText, useNotify } from './Feedback';
import { TextAreaField, TextField } from './fields/TextField';
import { FormDialog } from './FormDialog';

/** One sentence for an import result. */
export function importSummary(r: ZoneImportResult): string {
  const sets = r.recordSets ?? [];
  const into = sets.length === 1 ? ` into ${sets[0]}` : sets.length > 1 ? ` into ${sets.length} record sets` : '';
  return `${r.created ? `Created zone ${r.zoneName}: ` : ''}${r.endpoints} record${r.endpoints === 1 ? '' : 's'} imported${into}`;
}

/** The DNSZone object name of a zone apex ("Home.Example." → "home.example"). */
export function zoneObjectName(apex: string): string {
  return apex.trim().replace(/\.$/, '').toLowerCase();
}

/** The $ORIGIN of a zone file, if it declares one. */
export function originOf(text: string): string | undefined {
  const m = /^\$ORIGIN\s+(\S+)/im.exec(text);
  return m?.[1];
}

export function ZoneImportDialog({ isOpen, onClose }: { isOpen: boolean; onClose: () => void }) {
  const api = useApi();
  const qc = useQueryClient();
  const notify = useNotify();
  const navigate = useNavigate();
  const [file, setFile] = useState<File | null>(null);
  const [apex, setApex] = useState('');
  const [text, setText] = useState('');
  const [err, setErr] = useState<string>();
  const [result, setResult] = useState<ZoneImportResult | null>(null);
  const name = zoneObjectName(apex);
  const m = useMutation({
    mutationFn: () => api.zones.importBind(name, text, `${name}.`),
    onSuccess: (r) => {
      setResult(r);
      notify.success(importSummary(r));
      void qc.invalidateQueries({ queryKey: qk.zones });
    },
  });
  const close = () => {
    setFile(null);
    setApex('');
    setText('');
    setErr(undefined);
    setResult(null);
    m.reset();
    onClose();
  };
  const onFile = (files: File | File[] | null) => {
    const f = Array.isArray(files) ? (files[0] ?? null) : files;
    setFile(f);
    if (!f) return;
    void f.text().then((t) => {
      setText(t);
      const o = originOf(t);
      if (o && !apex.trim()) setApex(o);
    });
  };
  const submit = () => {
    const he = hostError(apex);
    const e = he ?? (nameError(name) ? 'Use lower-case letters, digits, "-" and "."' : undefined);
    setErr(e ?? undefined);
    if (!e && text.trim()) m.mutate();
  };
  return (
    <FormDialog
      isOpen={isOpen}
      onClose={close}
      title="Import a zone file"
      subtitle="Creates the zone from a BIND master file"
      width={720}
      noValidate
      onSubmit={submit}
      footer={
        <>
          <Button label="Close" variant="ghost" onClick={close} />
          {result ? (
            <Button
              label="Open the zone"
              variant="primary"
              onClick={() => {
                const z = result.zone;
                close();
                navigate(`/zones/${encodeURIComponent(z)}`);
              }}
            />
          ) : (
            <Button type="submit" label="Import" variant="primary" isDisabled={!text.trim()} isLoading={m.isPending} />
          )}
        </>
      }
    >
      <FileInput label="Zone file" value={file} onChange={onFile} accept=".zone,.db,.txt,text/plain,text/dns" />
      <TextField label="Zone apex" placeholder="home.arpa" help="Taken from $ORIGIN when the file has one" value={apex} onChange={setApex} isRequired error={err} />
      <TextAreaField label="Or paste the zone text" value={text} onChange={setText} rows={12} hasSpellCheck={false} placeholder={'$ORIGIN home.arpa.\n@ 3600 IN SOA ns1 hostmaster 1 7200 3600 1209600 300\nwww 300 IN A 192.0.2.10'} />
      <Text type="supporting" display="block">
        The import is all or nothing: a line that does not parse rejects the file. Importing into an existing zone replaces its earlier import; use Import BIND
        on the zone page for that.
      </Text>
      {m.error ? <Banner status="error" title="Import failed" description={errorText(m.error)} /> : null}
      {result ? (
        <Banner status={result.warnings.length ? 'warning' : 'success'} title={importSummary(result)} description={result.warnings.length ? result.warnings.join('; ') : undefined} />
      ) : null}
    </FormDialog>
  );
}
