import { Component, type ErrorInfo, type ReactNode } from 'react';
import { t } from '../i18n';

interface Props { children: ReactNode; }
interface State { failed: boolean; }

export class ErrorBoundary extends Component<Props, State> {
  state: State = { failed: false };

  static getDerivedStateFromError(): State {
    return { failed: true };
  }

  componentDidCatch(error: Error, info: ErrorInfo): void {
    console.error('UI error', error?.message, info.componentStack);
  }

  render(): ReactNode {
    if (this.state.failed) {
      return (
        <div className="banner">
          <h2>{t('common.error')}</h2>
          <p>{t('error.generic')}</p>
          <button className="btn small" type="button" onClick={() => window.location.reload()}>{t('common.retry')}</button>
        </div>
      );
    }
    return this.props.children;
  }
}
