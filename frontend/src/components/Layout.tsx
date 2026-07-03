import { Suspense, type ReactNode } from 'react';
import { NavLink, Outlet, useLocation } from 'react-router-dom';
import { Activity, BookOpen, Boxes, Braces, Layers, LogOut, Plug, Settings, Users } from 'lucide-react';
import { useSession } from '../lib/sessionContext';
import styles from './Layout.module.css';

const NAV_TOP = [
  { to: '/', label: 'Overview', icon: Activity, end: true },
  { to: '/services', label: 'Services', icon: Layers, end: false },
  { to: '/providers', label: 'Providers', icon: Plug, end: false },
  { to: '/users', label: 'Users', icon: Users, end: false },
] as const;

const DOCS_SUB = [
  { to: '/docs/api', label: 'API', icon: Braces },
  { to: '/docs/mcp', label: 'MCP', icon: Boxes },
] as const;

const navItemClass = ({ isActive }: { isActive: boolean }) =>
  isActive ? `${styles.navItem} ${styles.navItemActive}` : styles.navItem;

const subNavItemClass = ({ isActive }: { isActive: boolean }) =>
  isActive
    ? `${styles.navItem} ${styles.subNavItem} ${styles.navItemActive}`
    : `${styles.navItem} ${styles.subNavItem}`;

export function Layout() {
  // The Docs item is collapsed until any /docs route is active, then it expands
  // to reveal its second-level pages.
  const { pathname } = useLocation();
  const docsActive = pathname === '/docs' || pathname.startsWith('/docs/');
  const { me, signOut } = useSession();
  const isUser = me.role === 'user';

  return (
    <div className={styles.shell}>
      <aside className={styles.sidebar}>
        <div className={styles.brand}>
          <img src="/songguo-mark.svg" alt="" />
          <span className={styles.wordmark}>Songguo</span>
        </div>
        <nav className={styles.nav}>
          {isUser ? (
            // Scoped playground: models list only, no operator surfaces.
            <NavLink to="/services" className={navItemClass}>
              <Layers size={16} />
              <span>Models</span>
            </NavLink>
          ) : (
            <>
              {NAV_TOP.map(({ to, label, icon: Icon, end }) => (
                <NavLink key={to} to={to} end={end} className={navItemClass}>
                  <Icon size={16} />
                  <span>{label}</span>
                </NavLink>
              ))}

              <NavLink to="/docs" className={navItemClass}>
                <BookOpen size={16} />
                <span>Docs</span>
              </NavLink>
              {docsActive ? (
                <div className={styles.subNav}>
                  {DOCS_SUB.map(({ to, label, icon: Icon }) => (
                    <NavLink key={to} to={to} className={subNavItemClass}>
                      <Icon size={16} />
                      <span>{label}</span>
                    </NavLink>
                  ))}
                </div>
              ) : null}

              <NavLink to="/settings" className={navItemClass}>
                <Settings size={16} />
                <span>Settings</span>
              </NavLink>
            </>
          )}

          {isUser ? (
            // The user shell has no Settings page, so sign-out lives here.
            <button
              type="button"
              className={styles.navItem}
              onClick={signOut}
              style={{
                background: 'none',
                border: 'none',
                width: '100%',
                font: 'inherit',
                textAlign: 'left',
                cursor: 'pointer',
              }}
            >
              <LogOut size={16} />
              <span>Sign out</span>
            </button>
          ) : null}
        </nav>
      </aside>
      <main className={styles.main}>
        {/* Each route is a lazy chunk; keep the shell and show a spinner in the
            page area while the chunk loads. */}
        <Suspense
          fallback={
            <div className={styles.routeFallback}>
              <span className="spinner" />
            </div>
          }
        >
          <Outlet />
        </Suspense>
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
