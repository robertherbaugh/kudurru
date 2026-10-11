// Add or edit one source of a BlockList in a short dialog: a type choice
// (URL, pasted entries, an uploaded file, a ConfigMap) and the typed fields
// of that type. Inline entries of an existing source are not edited here;
// the entries browser adds and removes them without a giant text box.
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { FileInput } from '@astryxdesign/core/FileInput';
import { Grid } from '@astryxdesign/core/Grid';
import { SegmentedControl, SegmentedControlItem } from '@astryxdesign/core/SegmentedControl';
import { Text } from '@astryxdesign/core/Text';
import { useState } from 'react';
import type { ListFormat, ListSource } from '../../api/types';
import { formatExact } from '../../lib/format';
import { entriesProblem, FORMAT_OPTIONS, guessFormat, isHTTPURL, parseEntries, sourceKind } from '../../lib/lists';
import { EnumSelect } from '../fields/EnumSelect';
import { FieldShell } from '../fields/FieldHelp';
import { SwitchField, TextAreaField, TextField } from '../fields/TextField';
import { errorText } from '../Feedback';
import { FormDialog } from '../FormDialog';

export type SourceChoice = 'url' | 'paste' | 'file' | 'configMap';

/** More inline entries than this are better published at a URL. */
export const LARGE_INLINE = 50_000;

const NAME = /^[A-Za-z0-9]([A-Za-z0-9._-]{0,61}[A-Za-z0-9])?$/;

export function sourceNameError(name: string, taken: string[]): string | null {
  const n = name.trim();
  if (!n) return 'Required: the entries API and the status name the source by it';
  if (!NAME.test(n)) return 'Letters, digits, ".", "_" and "-", at most 63 characters';
  if (taken.includes(n)) return 'Another source of this list has this name';
  return null;
}

const GENERIC_FILES = new Set(['', 'hosts', 'list', 'lists', 'domains', 'filter', 'filters', 'index', 'blocklist', 'adblock', 'all']);

/**
 * A source name suggestion from a URL: the publisher and the file
 * ("…githubusercontent.com/hagezi/dns-blocklists/main/adblock/tif.txt" →
 * "hagezi-tif", ".../StevenBlack/hosts/master/hosts" → "stevenblack-hosts",
 * "https://small.oisd.nl/" → "small").
 */
export function suggestName(url: string, taken: string[]): string {
  let base = 'source';
  try {
    const u = new URL(url);
    const parts = u.pathname.split('/').filter(Boolean);
    const stem = (parts[parts.length - 1] ?? '').replace(/\.[a-z0-9]+$/i, '').toLowerCase();
    const github = u.hostname.includes('githubusercontent') || u.hostname.endsWith('github.io');
    const publisher = github && parts.length >= 2 ? (parts[0] ?? '') : (u.hostname.replace(/^(www|raw)\./, '').split('.')[0] ?? '');
    if (!GENERIC_FILES.has(stem) && parts.length > 1) base = `${publisher}-${stem}`;
    else if (github && parts.length >= 2) base = `${parts[0]}-${parts[1]}`;
    else base = publisher || base;
  } catch {
    /* not a URL yet */
  }
  return slugName(base, taken);
}

/** base as a source name (lower case, safe characters), made unique with -2, -3, … */
export function slugName(base: string, taken: string[]): string {
  const slug = base.toLowerCase().replace(/[^a-z0-9._-]+/g, '-').replace(/^[-._]+|[-._]+$/g, '').slice(0, 50) || 'source';
  let name = slug;
  for (let i = 2; taken.includes(name); i++) name = `${slug}-${i}`;
  return name;
}

export interface SourceDialogProps {
  open: boolean;
  onClose: () => void;
  /** The source to edit; undefined adds one. */
  initial?: ListSource;
  /** Names of the list's other sources. */
  taken: string[];
  onSave: (src: ListSource) => Promise<void>;
}

/** Keeps its fields in state: give it a new `key` per opening so each one starts fresh. */
export function SourceDialog({ open, onClose, initial, taken, onSave }: SourceDialogProps) {
  const editing = !!initial;
  const initialKind = initial ? sourceKind(initial) : 'url';
  const [choice, setChoice] = useState<SourceChoice>(initialKind === 'inline' ? 'paste' : initialKind);
  const [name, setName] = useState(initial?.name ?? '');
  const [nameTouched, setNameTouched] = useState(editing);
  const [url, setUrl] = useState(initial?.url ?? '');
  const [format, setFormat] = useState<ListFormat>(initial?.format ?? 'auto');
  const [formatTouched, setFormatTouched] = useState(editing);
  const [subdomains, setSubdomains] = useState(initial?.subdomains === true);
  const [text, setText] = useState('');
  const [file, setFile] = useState<File | null>(null);
  const [cmName, setCmName] = useState(initial?.configMapRef?.name ?? '');
  const [cmNamespace, setCmNamespace] = useState(initial?.configMapRef?.namespace ?? '');
  const [showErrors, setShowErrors] = useState(false);
  const [saveError, setSaveError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const inlineChoice = choice === 'paste' || choice === 'file';
  const entries = inlineChoice && !editing ? parseEntries(text) : [];
  const problem = entries.length ? entriesProblem(entries, format) : null;

  const errors: Record<string, string | null> = {
    name: sourceNameError(name, taken),
    url: choice === 'url' ? (isHTTPURL(url) ? null : 'An http(s) URL such as https://example.com/hosts.txt') : null,
    entries:
      inlineChoice && !editing
        ? !entries.length
          ? choice === 'file'
            ? 'Choose a text file with one entry per line'
            : 'Paste at least one entry'
          : problem
            ? `${formatExact(problem.count)} invalid, first "${problem.first}": ${problem.error}`
            : null
        : null,
    cm: choice === 'configMap' && !/^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$/.test(cmName.trim()) ? 'A ConfigMap name (lower-case RFC 1123)' : null,
  };
  const valid = Object.values(errors).every((e) => !e);
  const err = (k: string) => (showErrors ? (errors[k] ?? undefined) : undefined);

  const build = (): ListSource => {
    const base: ListSource = { name: name.trim(), format };
    if (subdomains) base.subdomains = true;
    if (initial?.enabled === false) base.enabled = false;
    switch (choice) {
      case 'url':
        return { ...base, url: url.trim() };
      case 'configMap':
        return { ...base, configMapRef: { name: cmName.trim(), ...(cmNamespace.trim() ? { namespace: cmNamespace.trim() } : {}) } };
      default:
        return { ...base, inline: editing ? (initial?.inline ?? []) : entries };
    }
  };

  const submit = async () => {
    setShowErrors(true);
    if (!valid) return;
    setBusy(true);
    setSaveError(null);
    try {
      await onSave(build());
      onClose();
    } catch (e) {
      setSaveError(errorText(e));
    } finally {
      setBusy(false);
    }
  };

  const onUrl = (v: string) => {
    setUrl(v);
    if (!formatTouched) setFormat(guessFormat(v));
    if (!nameTouched && isHTTPURL(v)) setName(suggestName(v, taken));
  };

  const onFile = (picked: File | File[] | null) => {
    const f = Array.isArray(picked) ? (picked[0] ?? null) : picked;
    setFile(f);
    if (!f) {
      setText('');
      return;
    }
    if (!nameTouched) setName(slugName(f.name.replace(/\.[a-z0-9]+$/i, ''), taken));
    void f.text().then(setText);
  };

  const formatHelp = editing || choice !== 'url' || formatTouched || format === 'auto' ? undefined : 'Picked from the URL; change it if the list says otherwise.';

  return (
    <FormDialog
      isOpen={open}
      onClose={onClose}
      title={editing ? `Edit source ${initial?.name ?? ''}`.trim() : 'Add source'}
      width={600}
      noValidate
      onSubmit={() => void submit()}
      footer={
        <>
          <Button label="Cancel" variant="ghost" onClick={onClose} />
          <Button type="submit" label={editing ? 'Save source' : 'Add source'} variant="primary" isLoading={busy} />
        </>
      }
    >
      {!editing ? (
        <SegmentedControl label="Source type" value={choice} onChange={(v) => setChoice(v as SourceChoice)}>
          <SegmentedControlItem value="url" label="URL" />
          <SegmentedControlItem value="paste" label="Paste entries" />
          <SegmentedControlItem value="file" label="Upload file" />
          <SegmentedControlItem value="configMap" label="ConfigMap" />
        </SegmentedControl>
      ) : null}

      {choice === 'url' ? (
        <TextField
          label="URL"
          help="Fetched by the leader on every refresh interval; replicas pull the body from it."
          placeholder="https://raw.githubusercontent.com/StevenBlack/hosts/master/hosts"
          value={url}
          onChange={onUrl}
          error={err('url')}
        />
      ) : null}

      {choice === 'paste' && !editing ? (
        <TextAreaField
          label="Entries"
          help="One per line in the format below; # and ! lines are comments."
          placeholder={'ads.example.com\n||tracker.example^\n0.0.0.0 telemetry.example'}
          rows={6}
          hasSpellCheck={false}
          value={text}
          onChange={setText}
          error={err('entries')}
        />
      ) : null}

      {choice === 'file' && !editing ? (
        <FieldShell
          help={
            <>
              Read in the browser and stored as inline entries of the list object.
              {file && entries.length ? (
                <Text type="inherit" color="inherit" display="block" role="status">
                  {formatExact(entries.length)} entries read from {file.name}.
                </Text>
              ) : null}
            </>
          }
          error={err('entries')}
        >
          {(status) => <FileInput label="Text file" value={file} onChange={onFile} accept=".txt,.list,.hosts,.conf,text/plain" status={status} />}
        </FieldShell>
      ) : null}

      {inlineChoice && !editing && entries.length > LARGE_INLINE ? (
        <Banner status="warning" title="A large inline source" description={`${formatExact(entries.length)} entries are stored in the list object itself; a list this big is better served from a URL.`} />
      ) : null}

      {inlineChoice && editing ? (
        <p className="k-muted k-small">{formatExact(initial?.inline?.length ?? 0)} inline entries. Browse the entries from the sources table to add or remove some.</p>
      ) : null}

      {choice === 'configMap' ? (
        <Grid columns={{ minWidth: 220, repeat: 'fit' }} gap={3} align="start">
          <TextField label="ConfigMap" placeholder="family-lists" value={cmName} onChange={setCmName} error={err('cm')} help="Kubernetes deployments only; every data key is read." />
          <TextField label="Namespace" placeholder="the list's namespace" value={cmNamespace} onChange={setCmNamespace} isOptional />
        </Grid>
      ) : null}

      <Grid columns={{ minWidth: 220, repeat: 'fit' }} gap={3} align="start">
        <TextField
          label="Name"
          help="Shown in the status and the query log."
          placeholder="main"
          value={name}
          onChange={(v) => {
            setName(v);
            setNameTouched(true);
          }}
          error={err('name')}
        />
        <EnumSelect
          label="Format"
          value={format}
          options={FORMAT_OPTIONS}
          description={formatHelp}
          onChange={(v) => {
            setFormat(v as ListFormat);
            setFormatTouched(true);
          }}
        />
      </Grid>
      <SwitchField
        label="Include subdomains"
        help='Hosts and plain-domain entries also match their subdomains, for lists published as "domains with subdomains". Adblock and wildcard rules are not affected.'
        value={subdomains}
        onChange={setSubdomains}
      />
      {saveError ? <Banner status="error" title="Could not save the source" description={saveError} /> : null}
    </FormDialog>
  );
}
