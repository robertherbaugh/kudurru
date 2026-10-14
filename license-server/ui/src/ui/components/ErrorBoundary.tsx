// Copied from web/src/components/ErrorBoundary.tsx at dd3dcb0; keep in step by hand.
// Changes: the console prefix says license portal.
// Catches render errors of one page so the shell and navigation survive.
import { Component, type ErrorInfo, type ReactNode } from 'react';
import { Banner } from '@astryxdesign/core/Banner';
import { Page } from './layout/Page';

interface State {
  error: Error | null;
}

export class ErrorBoundary extends Component<{ children: ReactNode }, State> {
  override state: State = { error: null };

  static getDerivedStateFromError(error: Error): State {
    return { error };
  }

  override componentDidCatch(error: Error, info: ErrorInfo): void {
    console.error('kudurru license portal: page crashed', error, info.componentStack);
  }

  override render(): ReactNode {
    if (this.state.error) {
      return (
        <Page>
          <Banner status="error" title="This page failed to render" description={this.state.error.message} />
        </Page>
      );
    }
    return this.props.children;
  }
}
