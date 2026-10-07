// Ported from CXC v0.2.40 plugins/codexclaw/gui/src/App.tsx (1-90), modified: the nav is
// the CRW route table, the provider display and the five CXC screens are gone, the
// sidebar tail carries the run-state bar, #/helper-roles and #/status render their own
// screens, and every other route still renders a placeholder until the issue that owns
// that screen lands.
import { ROUTES, navigate, routeFor, useRoute } from "./router.ts";
import { ToastHost } from "./ui/toast.tsx";
import { Icon } from "./ui/icons.tsx";
import { EmptyState } from "./ui/kit.tsx";
import { HelpDrawer, HelpTopicButton, useHelp } from "./ui/help.tsx";
import { HelperRolesPage } from "./pages/HelperRoles.tsx";
import { RunStateBar, useRunState } from "./components/RunStateBar.tsx";
import { Status } from "./pages/Status.tsx";

export function App() {
  const route = useRoute();
  const active = routeFor(route);
  const { helpOpen, helpTopic, openHelp, closeHelp } = useHelp(active.topic);
  // One poller for the whole shell: the sidebar bar and the status screen read the same
  // document, so one request per interval serves both.
  const runState = useRunState();

  return (
    <div className="shell">
      <aside className="sidebar">
        <div className="brand">
          <span className="logo" aria-hidden>
            <Icon name="activity" size={14} />
          </span>
          crw
        </div>
        <nav aria-label="Primary">
          {ROUTES.map((item) => (
            <button
              key={item.path}
              type="button"
              className={`nav-link ${active.path === item.path ? "active" : ""}`}
              aria-current={active.path === item.path ? "page" : undefined}
              onClick={() => navigate(item.path)}
            >
              <span className="ico"><Icon name={item.icon} size={16} /></span>
              {item.label}
            </button>
          ))}
        </nav>
        <div className="spacer" />
        {/* The status bar. The relay store, the App Server and the execution policy are
            read separately here, never folded into one green dot. */}
        <div className="foot">
          <RunStateBar view={runState} />
        </div>
      </aside>

      <main className="main">
        {active.path === "/helper-roles" ? (
          <HelperRolesPage />
        ) : (
          <>
            <div className="page-header">
              <span className="page-header-title">{active.label}</span>
              <HelpTopicButton topic={active.topic} onOpen={openHelp} />
            </div>
            <div className="page-head">
              <div>
                <h1>{active.label}</h1>
                <div className="sub">{active.subtitle}</div>
              </div>
            </div>
            <div className="page-body">
              {active.path === "/status" ? (
                <Status view={runState} />
              ) : (
                <EmptyState icon={active.icon} title="Not available yet">
                  This screen arrives with the issue that implements it.
                </EmptyState>
              )}
            </div>
          </>
        )}
      </main>
      <ToastHost />
      <HelpDrawer open={helpOpen} topic={helpTopic} onClose={closeHelp} />
    </div>
  );
}
