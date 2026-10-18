// /orders (orders.read): every Order Form (Schedule A of the commercial
// agreement), open ones first (draft, sent, accepted), then the newest,
// filtered by state in the URL. "New Order Form" opens the wizard
// (?new=1); each row links to the form's page.
import { Button } from '@astryxdesign/core/Button';
import { HStack } from '@astryxdesign/core/Stack';
import { Table } from '@astryxdesign/core/Table';
import { Text } from '@astryxdesign/core/Text';
import { Plus } from 'lucide-react';
import type { OrderFormStatus } from '@shared/api';
import { useCan } from '../api/context';
import { useOrderForms } from '../api/hooks';
import { orderFormColumns, type OrderFormRow } from '../components/orderFormColumns';
import { OrderFormWizardDialog } from '../components/OrderFormWizard';
import { ORDER_FORM_STATUS_LABEL, ORDER_FORM_STATUSES } from '../lib/words';
import { ErrorBanner, Loading } from '../ui/components/Feedback';
import { Page, PageHeader, Panel } from '../ui/components/layout/Page';
import { SelectField } from '../ui/fields/SelectField';
import { useNewParam, useSearchParams } from '../ui/lib/router';

const columns = orderFormColumns();

export default function OrderFormsPage() {
  const can = useCan();
  const [sp, setParams] = useSearchParams();
  const [wizard, setWizard] = useNewParam();
  const status = (sp.get('status') ?? '') as OrderFormStatus | '';
  const q = useOrderForms(status ? { status } : {});
  const write = can('orders.write');
  return (
    <Page>
      <PageHeader
        title="Order Forms"
        description="Schedule A of the Kudurru Commercial Software License Agreement: drafted here, accepted by the customer's authorized contacts in the portal, countersigned, then licensed."
        actions={write ? <Button label="New Order Form" variant="primary" icon={<Plus size={16} aria-hidden="true" />} onClick={() => setWizard(true)} /> : null}
      />
      <Panel title="Order Forms">
        <HStack gap={2}>
          <SelectField
            label="State"
            value={status}
            emptyLabel="Every state, open first"
            options={ORDER_FORM_STATUSES.map((s) => ({ value: s, label: ORDER_FORM_STATUS_LABEL[s] }))}
            onChange={(v) => setParams({ status: v })}
            width={300}
          />
        </HStack>
        {q.error ? <ErrorBanner error={q.error} /> : null}
        {q.isPending ? <Loading /> : null}
        {q.data ? (
          <Table<OrderFormRow>
            data={q.data.items as OrderFormRow[]}
            columns={columns}
            idKey="id"
            density="compact"
            verticalAlign="top"
            emptyState={<Text color="secondary">{status ? 'No Order Form in this state.' : 'No Order Forms yet. Draft the first one.'}</Text>}
          />
        ) : null}
      </Panel>
      <OrderFormWizardDialog isOpen={wizard && write} onClose={() => setWizard(false)} />
    </Page>
  );
}
