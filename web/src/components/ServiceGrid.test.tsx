import { render } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { CategoryIcon } from './ServiceGrid';

describe('CategoryIcon', () => {
  // A catalogue document names category icons; inherited object keys such
  // as "constructor" must fall back to the default icon, not render Object.
  it.each(['constructor', 'toString', 'no-such-icon', undefined])('renders %s as an icon', (icon) => {
    const { container } = render(<CategoryIcon icon={icon} />);
    expect(container.querySelector('svg')).not.toBeNull();
  });
});
