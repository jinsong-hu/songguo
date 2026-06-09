import { AlertTriangle } from 'lucide-react';
import styles from './ErrorBanner.module.css';

interface ErrorBannerProps {
  message: string;
  onRetry?: () => void;
}

export function ErrorBanner({ message, onRetry }: ErrorBannerProps) {
  return (
    <div className={styles.banner} role="alert">
      <AlertTriangle size={16} className={styles.icon} />
      <span className={styles.msg}>{message}</span>
      {onRetry && (
        <button className={styles.retry} onClick={onRetry}>
          Retry
        </button>
      )}
    </div>
  );
}
