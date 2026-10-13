// "Import key" (POST /api/v1/licenses/import): a FormDialog for a key
// signed by hand with licensegen before the portal existed. The server
// verifies it against the trusted keyring, refuses a License ID it already
// holds and records the license from the key's claims for the chosen
// customer; the dialog then opens the imported license.
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { useState } from 'react';
import { useFieldErrors } from '../api/fieldErrors';
import { useRouteMutation } from '../api/hooks';
import { useNotify } from '../ui/components/Feedback';
import { DialogForm, FormDialog } from '../ui/components/FormDialog';
import { CustomerField, type CustomerChoice } from '../ui/fields/CustomerField';
import { TextAreaField } from '../ui/fields/TextField';
import { useNavigate } from '../ui/lib/router';

export function ImportKeyDialog({ isOpen, onClose }: { isOpen: boolean; onClose: () => void }) {
  return (
    <FormDialog isOpen={isOpen} onClose={onClose} title="Import a key" subtitle="For keys signed with licensegen before the portal existed." width={600}>
      <ImportForm onClose={onClose} />
    </FormDialog>
  );
}

/** The body fields the controls show (a 400 names them inline). */
const IMPORT_FIELDS = ['token', 'customerId'];

function ImportForm({ onClose }: { onClose: () => void }) {
  const notify = useNotify();
  const navigate = useNavigate();
  const m = useRouteMutation('licenseImport');
  const [token, setToken] = useState('');
  const [customer, setCustomer] = useState<CustomerChoice | null>(null);
  const [showErrors, setShowErrors] = useState(false);
  const fe = useFieldErrors(m.error, IMPORT_FIELDS);
  const tokenError = token.trim() ? null : 'Paste the key';
  const customerError = customer ? null : 'Choose the customer the license belongs to';

  const submit = () => {
    setShowErrors(true);
    if (tokenError || customerError || !customer) return;
    m.mutate(
      { body: { token: token.replace(/\s+/g, ''), customerId: customer.id } },
      {
        onSuccess: (d) => {
          notify.success(`Imported ${d.id}`);
          onClose();
          navigate(`/licenses/${encodeURIComponent(d.id)}`);
        },
      },
    );
  };

  return (
    <DialogForm
      onSubmit={submit}
      noValidate
      gap={3}
      footer={
        <>
          <Button label="Cancel" variant="ghost" onClick={onClose} />
          <Button type="submit" label="Import" variant="primary" isLoading={m.isPending} />
        </>
      }
    >
      <TextAreaField
        label="Key"
        mono
        value={token}
        onChange={(v) => {
          setToken(v);
          fe.edited('token');
        }}
        isRequired
        help="The whole key, as licensegen printed it; line breaks are ignored as on the nodes."
        error={(showErrors ? tokenError : null) ?? fe.field('token')}
      />
      <CustomerField
        value={customer}
        onChange={(c) => {
          setCustomer(c);
          fe.edited('customerId');
        }}
        isRequired
        error={(showErrors ? customerError : null) ?? fe.field('customerId')}
      />
      {fe.summary ? <Banner status="error" title="The key was not imported" description={fe.summary} /> : null}
    </DialogForm>
  );
}
