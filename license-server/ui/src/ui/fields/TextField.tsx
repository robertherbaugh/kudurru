// Copied from dashboard/src/components/fields/TextField.tsx at dd3dcb0; keep in step by hand.
// Changes: none.
// Text, multi-line text, switch and checkbox fields: the Astryx control
// with its own label and the help under it (FieldHelp.tsx). Every other
// prop passes through to the Astryx component. `mono` sets the control in
// the monospace face (domain names, addresses, keys).
import { CheckboxInput, type CheckboxInputProps } from '@astryxdesign/core/CheckboxInput';
import { Switch, type SwitchProps } from '@astryxdesign/core/Switch';
import { TextArea, type TextAreaProps } from '@astryxdesign/core/TextArea';
import { TextInput, type TextInputProps } from '@astryxdesign/core/TextInput';
import { FieldShell, type HelpParts } from './FieldHelp';

type Own = 'description' | 'status' | 'statusVariant';

export type TextFieldProps = Omit<TextInputProps, Own> &
  HelpParts & {
    mono?: boolean | undefined;
    /** Field width (default: the column). */
    fieldWidth?: string | number | undefined;
  };

export function TextField({ help, error, warning, mono, fieldWidth, className, ...rest }: TextFieldProps) {
  return (
    <FieldShell help={help} error={error} warning={warning} width={fieldWidth}>
      {(status) => <TextInput {...rest} className={[mono ? 'k-mono' : '', className ?? ''].join(' ').trim() || undefined} status={status} />}
    </FieldShell>
  );
}

export type TextAreaFieldProps = Omit<TextAreaProps, Own> & HelpParts & { mono?: boolean | undefined };

export function TextAreaField({ help, error, warning, mono, className, ...rest }: TextAreaFieldProps) {
  return (
    <FieldShell help={help} error={error} warning={warning}>
      {(status) => <TextArea {...rest} className={[mono ? 'k-mono' : '', className ?? ''].join(' ').trim() || undefined} status={status} />}
    </FieldShell>
  );
}

// A switch or checkbox puts its label beside the control, so Astryx's own
// description already sits under the control row: these keep it, and only
// add the error as a detached status (the documented variant for them).
export type SwitchFieldProps = Omit<SwitchProps, 'description' | 'status'> & Omit<HelpParts, 'help'> & { help?: string | undefined };

export function SwitchField({ help, error, warning, ...rest }: SwitchFieldProps) {
  return (
    <FieldShell error={error} warning={warning}>
      {(status) => <Switch {...rest} description={help} status={status} />}
    </FieldShell>
  );
}

export type CheckboxFieldProps = Omit<CheckboxInputProps, 'description' | 'status'> & Omit<HelpParts, 'help'> & { help?: string | undefined };

export function CheckboxField({ help, error, warning, ...rest }: CheckboxFieldProps) {
  return (
    <FieldShell error={error} warning={warning}>
      {(status) => <CheckboxInput {...rest} description={help} status={status} />}
    </FieldShell>
  );
}
