// Copied from dashboard/src/components/FormDialog.tsx at dd3dcb0; keep in step by hand.
// Changes: none.
// The one modal frame every dashboard dialog uses: Astryx Dialog with the
// Layout it is designed for (DialogHeader with the title and the close
// button, LayoutContent that scrolls, LayoutFooter whose actions stay in
// view however long the form is).
//
// Dismissal: the close button and Escape always close. A click on the
// backdrop closes too while the dialog is pristine; once a field inside it
// has changed (any input or change event), only the close button, Escape
// and the footer buttons do, so a stray click cannot throw away a
// half-filled form. The DialogHeader title gets focus on open and names the
// dialog (aria-labelledby).
import { Dialog, DialogHeader } from '@astryxdesign/core/Dialog';
import { Layout, LayoutContent, LayoutFooter } from '@astryxdesign/core/Layout';
import { HStack, StackItem, VStack } from '@astryxdesign/core/Stack';
import { Text } from '@astryxdesign/core/Text';
import { createContext, useContext, useId, useState, type FormEvent, type ReactNode } from 'react';

export interface FrameContext {
  title: ReactNode;
  subtitle?: ReactNode;
  /** Unset for a dialog that cannot be dismissed (no close button). */
  onClose?: () => void;
  markDirty: () => void;
}

const Frame = createContext<FrameContext | null>(null);

/** The enclosing FormDialog's title, close and dirty tracking (null outside one). */
export function useDialogFrame(): FrameContext | null {
  return useContext(Frame);
}

export interface FormDialogProps {
  isOpen: boolean;
  /** Called by the close button, Escape and (while pristine) the backdrop. */
  onClose: () => void;
  title: ReactNode;
  subtitle?: ReactNode;
  /** Preferred width in pixels; the dialog never gets wider than the viewport. */
  width?: number;
  /**
   * Footer actions (right-aligned). With `onSubmit`, a `type="submit"`
   * button here submits the form. Leave both unset when the content renders
   * its own DialogForm (ObjectForm, RecordEditor).
   */
  footer?: ReactNode;
  /** Wraps header, body and footer in a <form> (Enter submits). */
  onSubmit?: () => void;
  noValidate?: boolean;
  /**
   * False for a mandatory step (a forced password change): no close
   * button, Escape and the backdrop do nothing, only the footer acts.
   */
  isDismissable?: boolean;
  /** Dirty whatever the input events say (a wizard's own state, a click-only choice). */
  isDirty?: boolean;
  /** Called when a field inside changes (the first input or change event and every later one). */
  onDirty?: () => void;
  children: ReactNode;
}

export function FormDialog({ isOpen, onClose, title, subtitle, width = 520, footer, onSubmit, noValidate, isDismissable = true, isDirty, onDirty, children }: FormDialogProps) {
  const [dirty, setDirty] = useState(false);
  // Every opening starts pristine (state adjusted during render, not in an effect).
  const [wasOpen, setWasOpen] = useState(isOpen);
  if (wasOpen !== isOpen) {
    setWasOpen(isOpen);
    if (!isOpen) setDirty(false);
  }
  // Named explicitly: Dialog only finds a DialogHeader title that is
  // already rendered when the <dialog> mounts, and most dialogs mount
  // closed with their content not rendered yet.
  const titleId = useId();
  const ctx: FrameContext = {
    title: (
      <Text type="inherit" color="inherit" id={titleId}>
        {title}
      </Text>
    ),
    subtitle,
    onClose: isDismissable ? onClose : undefined,
    markDirty: () => {
      setDirty(true);
      onDirty?.();
    },
  };
  const own = footer !== undefined || onSubmit !== undefined;
  return (
    <Dialog
      isOpen={isOpen}
      onOpenChange={(open) => {
        if (!open && isDismissable) onClose();
      }}
      purpose={!isDismissable ? 'required' : dirty || isDirty ? 'form' : 'info'}
      width={width}
      maxHeight="90dvh"
      aria-labelledby={titleId}
    >
      {/* Content only while open: a closed dialog keeps no hidden copies of
          its fields in the page (labels, ids and tests stay unambiguous). */}
      <Frame value={ctx}>
        {!isOpen ? null : own ? (
          <DialogForm onSubmit={onSubmit} noValidate={noValidate} footer={footer}>
            {children}
          </DialogForm>
        ) : (
          children
        )}
      </Frame>
    </Dialog>
  );
}

export interface DialogFormProps {
  /** Without it the frame is a plain group (an informational dialog). */
  onSubmit?: () => void;
  noValidate?: boolean;
  footer?: ReactNode;
  /** Space between the body's blocks (default 4; forms use 3). */
  gap?: 3 | 4;
  'aria-label'?: string;
  children: ReactNode;
}

/**
 * The body and the actions of a dialog. Inside a FormDialog it fills the
 * dialog layout; anywhere else (tests, a page) it renders a plain form with
 * the actions in a row below the fields.
 */
export function DialogForm({ onSubmit, noValidate, footer, gap = 4, children, ...aria }: DialogFormProps) {
  const frame = useContext(Frame);
  const submit = onSubmit
    ? (e: FormEvent) => {
        e.preventDefault();
        onSubmit();
      }
    : undefined;
  if (!frame) {
    const body = (
      <>
        {children}
        {footer ? <Actions>{footer}</Actions> : null}
      </>
    );
    return onSubmit ? (
      <VStack as="form" gap={gap} {...formAttrs(noValidate)} onSubmit={submit} {...aria}>
        {body}
      </VStack>
    ) : (
      <VStack gap={gap}>{body}</VStack>
    );
  }
  const layout = (
    <Layout
      header={
        <DialogHeader
          title={frame.title}
          subtitle={frame.subtitle}
          onOpenChange={frame.onClose ? (open) => !open && frame.onClose?.() : undefined}
          hasDivider
        />
      }
      content={
        <LayoutContent>
          <VStack gap={gap}>{children}</VStack>
        </LayoutContent>
      }
      footer={
        footer ? (
          <LayoutFooter hasDivider>
            <Actions>{footer}</Actions>
          </LayoutFooter>
        ) : undefined
      }
    />
  );
  // The frame fills the dialog (a flex column) so the Layout's content
  // scrolls and its footer stays in view.
  return onSubmit ? (
    <StackItem as="form" size="fill" {...formAttrs(noValidate)} onSubmit={submit} onInput={frame.markDirty} onChange={frame.markDirty} {...aria}>
      {layout}
    </StackItem>
  ) : (
    <StackItem size="fill" onInput={frame.markDirty} onChange={frame.markDirty}>
      {layout}
    </StackItem>
  );
}

/** <form> attributes the Astryx stack props do not type (they reach the element). */
function formAttrs(noValidate: boolean | undefined): object {
  return { noValidate };
}

/** Dialog actions: a right-aligned row that wraps on narrow screens. */
function Actions({ children }: { children: ReactNode }) {
  return (
    <HStack gap={2} hAlign="end" vAlign="center" wrap="wrap">
      {children}
    </HStack>
  );
}
