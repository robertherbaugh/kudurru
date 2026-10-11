// Import a BIND master file as a new zone: POST /zones/{zone}/import with
// ?origin= creates the DNSZone from the file (the SOA and NS of the file
// become the zone's) and writes its records into API-managed record sets.
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { Dialog, DialogHeader } from '@astryxdesign/core/Dialog';
import { Layout, LayoutContent, LayoutFooter } from '@astryxdesign/core/Layout';
import { TextArea } from '@astryxdesign/core/TextArea';
import { TextInput } from '@astryxdesign/core/TextInput';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { useId, useState, type ChangeEvent } from 'react';
import { qk, useApi } from '../api/context';
import type { ZoneImportResult } from '../api/types';
import { nameError } from '../lib/objectSchemas';
import { useNavigate } from '../lib/router';
import { hostError } from '../lib/rr';
import { errorText, useNotify } from './Feedback';

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
  const formId = useId();
  const fileId = useId();
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
    setApex('');
    setText('');
    setErr(undefined);
    setResult(null);
    m.reset();
    onClose();
  };
  const onFile = (e: ChangeEvent<HTMLInputElement>) => {
    const f = e.target.files?.[0];
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
    <Dialog isOpen={isOpen} onOpenChange={(o) => !o && close()} purpose="form" width={720} maxHeight="90dvh">
      <Layout
        header={<DialogHeader title="Import a zone file" subtitle="Creates the zone from a BIND master file" onOpenChange={(o) => !o && close()} />}
        content={
          <LayoutContent>
            <form
              id={formId}
              className="k-stack-lg"
              noValidate
              onSubmit={(e) => {
                e.preventDefault();
                submit();
              }}
            >
              <label className="k-label" htmlFor={fileId}>
                Zone file
                <input id={fileId} type="file" accept=".zone,.db,.txt,text/plain,text/dns" onChange={onFile} />
              </label>
              <TextInput
                label="Zone apex"
                placeholder="home.arpa"
                description="Taken from $ORIGIN when the file has one"
                value={apex}
                onChange={setApex}
                isRequired
                status={err ? { type: 'error', message: err } : undefined}
              />
              <TextArea label="Or paste the zone text" value={text} onChange={setText} rows={12} hasSpellCheck={false} placeholder={'$ORIGIN home.arpa.\n@ 3600 IN SOA ns1 hostmaster 1 7200 3600 1209600 300\nwww 300 IN A 192.0.2.10'} />
              <p className="k-muted k-small">
                The import is all or nothing: a line that does not parse rejects the file. Importing into an existing zone replaces its earlier import; use Import BIND
                on the zone page for that.
              </p>
              {m.error ? <Banner status="error" title="Import failed" description={errorText(m.error)} /> : null}
              {result ? (
                <Banner status={result.warnings.length ? 'warning' : 'success'} title={importSummary(result)} description={result.warnings.length ? result.warnings.join('; ') : undefined} />
              ) : null}
            </form>
          </LayoutContent>
        }
        footer={
          <LayoutFooter hasDivider>
            <div className="k-row">
              <span className="k-spacer" />
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
                <Button type="submit" form={formId} label="Import" variant="primary" isDisabled={!text.trim()} isLoading={m.isPending} />
              )}
            </div>
          </LayoutFooter>
        }
      />
    </Dialog>
  );
}
