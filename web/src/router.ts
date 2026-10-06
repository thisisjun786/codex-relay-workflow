// Ported from CXC v0.2.40 plugins/codexclaw/gui/src/router.ts (1-28), modified: the
// route set is CRW's (#/status, #/policy, #/helper-roles) with #/status as the default,
// the table is exported so a node:test file can assert it without loading JSX, and an
// unknown hash normalizes to the default instead of leaving the shell on an empty screen.
/**
 * router.ts - a dependency-free hash router.
 *
 * No react-router: the app has a handful of routes, so a `#/path` hash plus a subscribe
 * hook is enough and works from a static file or any serve origin without server-side
 * route configuration. The table lives here rather than in App.tsx so the CRW route set is
 * assertable from node:test, which cannot load .tsx; the two `import type` lines below are
 * erased by the type stripper, so importing this module never loads a JSX file.
 *
 * The subtitle lives here too: a screen reads it from the route it selected, so a route
 * cannot render without one.
 */
import { useEffect, useState } from "react";
import type { IconName } from "./ui/icons.tsx";
import type { HelpTopicId } from "./ui/help.tsx";

export interface Route {
  path: string;
  label: string;
  subtitle: string;
  icon: IconName;
  topic: HelpTopicId;
}

export const DEFAULT_ROUTE = "/status";

export const ROUTES: readonly Route[] = [
  {
    path: "/status",
    label: "Status",
    subtitle: "Relay delivery, parents, children, merge lanes, capacity and audit alerts.",
    icon: "activity",
    topic: "status",
  },
  {
    path: "/policy",
    label: "Execution policy",
    subtitle: "The supervisor, parent and child model and effort pairs, and their exceptions.",
    icon: "shield",
    topic: "policy",
  },
  {
    path: "/helper-roles",
    label: "Helper roles",
    subtitle: "The global explorer, reviewer, executor and architect settings.",
    icon: "sliders",
    topic: "helper-roles",
  },
];

/** The route for a hash path, normalized: an empty or unknown hash is the default. */
export function routeFor(path: string): Route {
  return ROUTES.find((route) => route.path === path) ?? ROUTES[0];
}

/** The hash as a route path, normalized: an empty or unknown hash is the default route. */
export function currentRoute(): string {
  const hash = typeof location !== "undefined" ? location.hash : "";
  const path = hash.replace(/^#/, "");
  return routeFor(path).path;
}

export function navigate(path: string): void {
  if (typeof location !== "undefined") location.hash = path;
}

export function useRoute(): string {
  const [route, setRoute] = useState<string>(currentRoute());
  useEffect(() => {
    const onChange = () => setRoute(currentRoute());
    window.addEventListener("hashchange", onChange);
    return () => window.removeEventListener("hashchange", onChange);
  }, []);
  return route;
}
