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
import { createContext, useContext, useId, useState, type FormEvent, type ReactNode } from 'react';

interface FrameContext {
  title: ReactNode;
  subtitle?: ReactNode;
  onClose: () => void;
  markDirty: () => void;
}

const Frame = createContext<FrameContext | null>(null);

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
  children: ReactNode;
}

export function FormDialog({ isOpen, onClose, title, subtitle, width = 520, footer, onSubmit, noValidate, children }: FormDialogProps) {
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
  const ctx: FrameContext = { title: <span id={titleId}>{title}</span>, subtitle, onClose, markDirty: () => setDirty(true) };
  const own = footer !== undefined || onSubmit !== undefined;
  return (
    <Dialog
      isOpen={isOpen}
      onOpenChange={(open) => {
        if (!open) onClose();
      }}
      purpose={dirty ? 'form' : 'info'}
      width={width}
      maxHeight="90dvh"
      aria-labelledby={titleId}
    >
      <Frame value={ctx}>
        {own ? (
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
  /** Without it the frame is a plain <div> (an informational dialog). */
  onSubmit?: () => void;
  noValidate?: boolean;
  footer?: ReactNode;
  className?: string;
  'aria-label'?: string;
  children: ReactNode;
}

/**
 * The body and the actions of a dialog. Inside a FormDialog it fills the
 * dialog layout; anywhere else (tests, a page) it renders a plain form with
 * the actions in a row below the fields.
 */
export function DialogForm({ onSubmit, noValidate, footer, className, children, ...aria }: DialogFormProps) {
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
        {footer ? <div className="k-dialog-actions">{footer}</div> : null}
      </>
    );
    return onSubmit ? (
      <form className={className} noValidate={noValidate} onSubmit={submit} {...aria}>
        {body}
      </form>
    ) : (
      <div className={className}>{body}</div>
    );
  }
  const layout = (
    <Layout
      header={<DialogHeader title={frame.title} subtitle={frame.subtitle} onOpenChange={(open) => !open && frame.onClose()} hasDivider />}
      content={
        <LayoutContent>
          <div className={className ?? 'k-stack-lg'}>{children}</div>
        </LayoutContent>
      }
      footer={
        footer ? (
          <LayoutFooter hasDivider>
            <div className="k-dialog-actions">{footer}</div>
          </LayoutFooter>
        ) : undefined
      }
    />
  );
  return onSubmit ? (
    <form className="k-dialog-frame" noValidate={noValidate} onSubmit={submit} onInput={frame.markDirty} onChange={frame.markDirty} {...aria}>
      {layout}
    </form>
  ) : (
    <div className="k-dialog-frame" onInput={frame.markDirty} onChange={frame.markDirty}>
      {layout}
    </div>
  );
}
