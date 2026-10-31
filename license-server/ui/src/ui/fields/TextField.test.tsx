// `mono` puts the k-mono class on an element that wraps the native input
// or textarea, so the `.k-mono input` / `.k-mono textarea` rule in
// styles.css reaches it, and a caller's controlClassName is kept beside it.
import { render, screen } from '@testing-library/react';
import { TextAreaField, TextField } from './TextField';

describe('TextField mono', () => {
  it('wraps the input in k-mono and keeps the caller class', () => {
    render(<TextField label="Domain" value="example.com" mono controlClassName="extra" />);
    const input = screen.getByRole('textbox', { name: 'Domain' });
    const wrapper = input.closest('.k-mono');
    expect(wrapper).not.toBeNull();
    expect(wrapper).toHaveClass('extra');
  });

  it('wraps the textarea in k-mono', () => {
    render(<TextAreaField label="Key" value="abc" mono />);
    expect(screen.getByRole('textbox', { name: 'Key' }).closest('.k-mono')).not.toBeNull();
  });

  it('leaves the class off without mono', () => {
    render(<TextField label="Name" value="" />);
    expect(screen.getByRole('textbox', { name: 'Name' }).closest('.k-mono')).toBeNull();
  });
});
