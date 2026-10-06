// Ported from CXC v0.2.40 plugins/codexclaw/gui/src/ui/icons.tsx (1-117), modified: the
// two channel brand marks and their IconName members are removed (this port has no
// messenger screens); the generic stroke set is kept.
/**
 * icons.tsx - inline SVG icon set.
 *
 * NO emoji in UI: emoji-as-icon is the strongest AI-slop signal. Stroke icons are
 * Lucide-style (24 viewBox, stroke-width 2, round caps, currentColor). Zero-dep:
 * hand-inlined paths, no icon library import.
 */
import type { CSSProperties } from "react";

export type IconName =
  | "activity"
  | "database"
  | "link"
  | "cpu"
  | "sliders"
  | "inbox"
  | "alert"
  | "check"
  | "check-circle"
  | "x"
  | "arrow-right"
  | "copy"
  | "external"
  | "shield"
  | "chevron-left"
  | "chevron-right"
  | "settings";

interface IconProps {
  name: IconName;
  size?: number;
  className?: string;
  style?: CSSProperties;
  title?: string;
}

const STROKE: Record<IconName, string[]> = {
  activity: ["M22 12h-4l-3 9L9 3l-3 9H2"],
  database: [
    "M12 3c4.97 0 9 1.34 9 3s-4.03 3-9 3-9-1.34-9-3 4.03-3 9-3z",
    "M3 6v6c0 1.66 4.03 3 9 3s9-1.34 9-3V6",
    "M3 12v6c0 1.66 4.03 3 9 3s9-1.34 9-3v-6",
  ],
  link: ["M9 17H7A5 5 0 0 1 7 7h2", "M15 7h2a5 5 0 1 1 0 10h-2", "M8 12h8"],
  cpu: [
    "M6 6h12v12H6z",
    "M9 9h6v6H9z",
    "M9 2v2", "M15 2v2", "M9 20v2", "M15 20v2",
    "M2 9h2", "M2 15h2", "M20 9h2", "M20 15h2",
  ],
  sliders: [
    "M4 21v-7", "M4 10V3", "M12 21v-9", "M12 8V3", "M20 21v-5", "M20 12V3",
    "M2 14h4", "M10 8h4", "M18 16h4",
  ],
  inbox: [
    "M22 12h-6l-2 3h-4l-2-3H2",
    "M5.45 5.11 2 12v6a2 2 0 0 0 2 2h16a2 2 0 0 0 2-2v-6l-3.45-6.89A2 2 0 0 0 16.76 4H7.24a2 2 0 0 0-1.79 1.11z",
  ],
  alert: ["m21.73 18-8-14a2 2 0 0 0-3.48 0l-8 14A2 2 0 0 0 4 21h16a2 2 0 0 0 1.73-3Z", "M12 9v4", "M12 17h.01"],
  check: ["M20 6 9 17l-5-5"],
  "check-circle": ["M21.801 10A10 10 0 1 1 17 3.335", "m9 11 3 3L22 4"],
  x: ["M18 6 6 18", "M6 6l12 12"],
  "arrow-right": ["M5 12h14", "m12 5 7 7-7 7"],
  copy: [
    "M8 10a2 2 0 0 1 2-2h10a2 2 0 0 1 2 2v10a2 2 0 0 1-2 2H10a2 2 0 0 1-2-2z",
    "M4 16c-1.1 0-2-.9-2-2V4c0-1.1.9-2 2-2h10c1.1 0 2 .9 2 2",
  ],
  external: ["M15 3h6v6", "M10 14 21 3", "M18 13v6a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h6"],
  shield: ["M20 13c0 5-3.5 7.5-7.66 8.95a1 1 0 0 1-.67-.01C7.5 20.5 4 18 4 13V6a1 1 0 0 1 1-1c2 0 4.5-1.2 6.24-2.72a1.17 1.17 0 0 1 1.52 0C14.51 3.81 17 5 19 5a1 1 0 0 1 1 1z"],
  "chevron-left": ["m15 18-6-6 6-6"],
  "chevron-right": ["m9 18 6-6-6-6"],
  settings: [
    "M12.22 2h-.44a2 2 0 0 0-2 2v.18a2 2 0 0 1-1 1.73l-.43.25a2 2 0 0 1-2 0l-.15-.08a2 2 0 0 0-2.73.73l-.22.38a2 2 0 0 0 .73 2.73l.15.1a2 2 0 0 1 1 1.72v.51a2 2 0 0 1-1 1.74l-.15.09a2 2 0 0 0-.73 2.73l.22.38a2 2 0 0 0 2.73.73l.15-.08a2 2 0 0 1 2 0l.43.25a2 2 0 0 1 1 1.73V20a2 2 0 0 0 2 2h.44a2 2 0 0 0 2-2v-.18a2 2 0 0 1 1-1.73l.43-.25a2 2 0 0 1 2 0l.15.08a2 2 0 0 0 2.73-.73l.22-.39a2 2 0 0 0-.73-2.73l-.15-.08a2 2 0 0 1-1-1.74v-.5a2 2 0 0 1 1-1.74l.15-.09a2 2 0 0 0 .73-2.73l-.22-.38a2 2 0 0 0-2.73-.73l-.15.08a2 2 0 0 1-2 0l-.43-.25a2 2 0 0 1-1-1.73V4a2 2 0 0 0-2-2z",
    "M12 8a4 4 0 1 0 0 8 4 4 0 0 0 0-8z",
  ],
};

export function Icon({ name, size = 18, className, style, title }: IconProps) {
  const common = {
    width: size,
    height: size,
    viewBox: "0 0 24 24",
    className,
    style,
    role: title ? "img" : ("presentation" as const),
    "aria-label": title,
    "aria-hidden": title ? undefined : true,
  };
  return (
    <svg {...common} fill="none" stroke="currentColor" strokeWidth={2} strokeLinecap="round" strokeLinejoin="round">
      {title ? <title>{title}</title> : null}
      {STROKE[name].map((d, i) => (
        <path key={i} d={d} />
      ))}
    </svg>
  );
}
