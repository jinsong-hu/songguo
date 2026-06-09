import type { LucideIcon } from 'lucide-react';
import { Inbox } from 'lucide-react';
import type { ReactNode } from 'react';
import styles from './EmptyState.module.css';

interface EmptyStateProps {
  title: string;
  hint?: ReactNode;
  icon?: LucideIcon;
}

export function EmptyState({ title, hint, icon: Icon = Inbox }: EmptyStateProps) {
  return (
    <div className={styles.empty}>
      <span className={styles.icon}>
        <Icon size={20} />
      </span>
      <span className={styles.title}>{title}</span>
      {hint ? <span className={styles.hint}>{hint}</span> : null}
    </div>
  );
}
