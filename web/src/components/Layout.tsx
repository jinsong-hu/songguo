import type { ReactNode } from 'react';
import { NavLink, Outlet } from 'react-router-dom';
import { Activity, KeyRound, Lock, LockOpen, Server, Settings } from 'lucide-react';
import styles from './Layout.module.css';

interface LayoutContext {
  adminProtected: boolean;
  version: string;
}

const NAV = [
  { to: '/', label: 'Overview', icon: Activity, end: true },
  { to: '/vendors', label: 'Vendors', icon: Server, end: false },
  { to: '/tokens', label: 'Tokens', icon: KeyRound, end: false },
  { to: '/settings', label: 'Settings', icon: Settings, end: false },
] as const;

export function Layout({ adminProtected, version }: LayoutContext) {
  return (
    <div className={styles.shell}>
      <aside className={styles.sidebar}>
        <div className={styles.brand}>
          <img src="/songguo-mark.svg" alt="" />
          <span className={styles.wordmark}>Songguo</span>
        </div>
        <nav className={styles.nav}>
          {NAV.map(({ to, label, icon: Icon, end }) => (
            <NavLink
              key={to}
              to={to}
              end={end}
              className={({ isActive }) =>
                isActive ? `${styles.navItem} ${styles.navItemActive}` : styles.navItem
              }
            >
              <Icon size={16} />
              <span>{label}</span>
            </NavLink>
          ))}
        </nav>
        <div className={styles.footer}>
          <span
            className={`${styles.lock} ${adminProtected ? styles.lockProtected : styles.lockOpen}`}
          >
            {adminProtected ? <Lock size={13} /> : <LockOpen size={13} />}
            {adminProtected ? 'Protected' : 'Unprotected'}
          </span>
          <span className={styles.version}>v{version}</span>
        </div>
      </aside>
      <main className={styles.main}>
        <Outlet />
      </main>
    </div>
  );
}

interface PageProps {
  title: string;
  actions?: ReactNode;
  children: ReactNode;
}

/** Page renders the top toolbar (title + actions) and the scrolling body. */
export function Page({ title, actions, children }: PageProps) {
  return (
    <>
      <div className={styles.toolbar}>
        <h1 className={styles.pageTitle}>{title}</h1>
        {actions ? <div className={styles.toolbarActions}>{actions}</div> : null}
      </div>
      <div className={styles.body}>{children}</div>
    </>
  );
}
