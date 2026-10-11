// Catches render errors of one page so the shell and navigation survive.
import { Component, type ErrorInfo, type ReactNode } from 'react';
import { Banner } from '@astryxdesign/core/Banner';

interface State {
  error: Error | null;
}

export class ErrorBoundary extends Component<{ children: ReactNode }, State> {
  override state: State = { error: null };

  static getDerivedStateFromError(error: Error): State {
    return { error };
  }

  override componentDidCatch(error: Error, info: ErrorInfo): void {
    console.error('kudurru: page crashed', error, info.componentStack);
  }

  override render(): ReactNode {
    if (this.state.error) {
      return (
        <div className="k-page">
          <Banner status="error" title="This page failed to render" description={this.state.error.message} />
        </div>
      );
    }
    return this.props.children;
  }
}
