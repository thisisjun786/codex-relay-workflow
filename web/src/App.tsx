// Ported from CXC v0.2.40 plugins/codexclaw/gui/src/App.tsx (1-90), modified: the nav is
// the CRW route table, the provider display and the five CXC screens are gone, the
// sidebar tail keeps only the status-bar slot, and each route renders a placeholder until
// the issue that owns that screen lands.
import { ROUTES, navigate, routeFor, useRoute } from "./router.ts";
import { ToastHost } from "./ui/toast.tsx";
import { Icon } from "./ui/icons.tsx";
import { EmptyState } from "./ui/kit.tsx";
import { HelpDrawer, HelpTopicButton, useHelp } from "./ui/help.tsx";

export function App() {
  const route = useRoute();
  const active = routeFor(route);
  const { helpOpen, helpTopic, openHelp, closeHelp } = useHelp(active.topic);

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
        <div className="foot" aria-label="status bar" />
      </aside>

      <main className="main">
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
          <EmptyState icon={active.icon} title="Not available yet">
            This screen arrives with the issue that implements it.
          </EmptyState>
        </div>
      </main>
      <ToastHost />
      <HelpDrawer open={helpOpen} topic={helpTopic} onClose={closeHelp} />
    </div>
  );
}

