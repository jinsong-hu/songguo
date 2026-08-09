import { Suspense, type ReactNode } from 'react';
import { NavLink, Outlet, useLocation } from 'react-router-dom';
import {
  Activity,
  BarChart3,
  BookOpen,
  Boxes,
  Braces,
  Layers,
  LogOut,
  MonitorSmartphone,
  Plug,
  Settings,
  Users,
} from 'lucide-react';
import { useSession } from '../lib/sessionContext';
import styles from './Layout.module.css';

const NAV_TOP = [
  { to: '/', label: 'Overview', icon: Activity, end: true },
] as const;

// The same dashboard, pointed at one thing at a time. Overview answers "how is
// the gateway doing"; these answer "how are the providers / models / users /
// clients doing", which wants a different default breakdown and a different
// selection of panels rather than a different kind of page.
const INSIGHTS_SUB = [
  { to: '/insights/providers', label: 'Providers', icon: Plug },
  { to: '/insights/models', label: 'Models', icon: Boxes },
  { to: '/insights/users', label: 'Users', icon: Users },
  { to: '/insights/clients', label: 'Clients', icon: MonitorSmartphone },
] as const;

// Configuration surfaces, below the analytics ones. `Providers` and `Users`
// appear in both groups on purpose: the same nouns, once as "what is it doing"
// and once as "how is it set up".
const NAV_CONFIG = [
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
  // Docs and Insights are collapsed until one of their routes is active, then
  // they expand to reveal their second-level pages.
  const { pathname } = useLocation();
  const docsActive = pathname === '/docs' || pathname.startsWith('/docs/');
  const insightsActive = pathname === '/insights' || pathname.startsWith('/insights/');
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
            // Scoped shell: own-traffic Overview + the Services playground. No
            // operator surfaces.
            <>
              <NavLink to="/" end className={navItemClass}>
                <Activity size={16} />
                <span>Overview</span>
              </NavLink>
              <NavLink to="/services" className={navItemClass}>
                <Layers size={16} />
                <span>Services</span>
              </NavLink>
            </>
          ) : (
            <>
              {NAV_TOP.map(({ to, label, icon: Icon, end }) => (
                <NavLink key={to} to={to} end={end} className={navItemClass}>
                  <Icon size={16} />
                  <span>{label}</span>
                </NavLink>
              ))}

              <NavLink to="/insights" className={navItemClass}>
                <BarChart3 size={16} />
                <span>Insights</span>
              </NavLink>
              {insightsActive ? (
                <div className={styles.subNav}>
                  {INSIGHTS_SUB.map(({ to, label, icon: Icon }) => (
                    <NavLink key={to} to={to} className={subNavItemClass}>
                      <Icon size={16} />
                      <span>{label}</span>
                    </NavLink>
                  ))}
                </div>
              ) : null}

              {NAV_CONFIG.map(({ to, label, icon: Icon, end }) => (
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
  title: ReactNode;
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
