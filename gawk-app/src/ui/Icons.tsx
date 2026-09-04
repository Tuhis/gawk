// Inline UI icons for the production surfaces (public/icons.svg holds only
// social marks). All draw with currentColor so they inherit button color;
// sized 1em so they scale with font-size. Kept minimal and consistent
// (1.6px stroke, round caps/joins) to match the restrained design language.
import type { SVGProps } from 'react';

const base: SVGProps<SVGSVGElement> = {
  width: '1em',
  height: '1em',
  viewBox: '0 0 24 24',
  fill: 'none',
  stroke: 'currentColor',
  strokeWidth: 1.6,
  strokeLinecap: 'round',
  strokeLinejoin: 'round',
  'aria-hidden': true,
};

export function FullscreenIcon(props: SVGProps<SVGSVGElement>) {
  return (
    <svg {...base} {...props}>
      <path d="M4 9V5a1 1 0 0 1 1-1h4M15 4h4a1 1 0 0 1 1 1v4M20 15v4a1 1 0 0 1-1 1h-4M9 20H5a1 1 0 0 1-1-1v-4" />
    </svg>
  );
}

export function FullscreenExitIcon(props: SVGProps<SVGSVGElement>) {
  return (
    <svg {...base} {...props}>
      <path d="M9 4v4a1 1 0 0 1-1 1H4M20 9h-4a1 1 0 0 1-1-1V4M15 20v-4a1 1 0 0 1 1-1h4M4 15h4a1 1 0 0 1 1 1v4" />
    </svg>
  );
}

export function LeaveIcon(props: SVGProps<SVGSVGElement>) {
  // A power/exit glyph — leave the stream.
  return (
    <svg {...base} {...props}>
      <path d="M12 4v8M8.5 6.5a7 7 0 1 0 7 0" />
    </svg>
  );
}

export function GearIcon(props: SVGProps<SVGSVGElement>) {
  return (
    <svg {...base} {...props}>
      <circle cx="12" cy="12" r="3" />
      <path d="M12 2.5v2M12 19.5v2M2.5 12h2M19.5 12h2M5.1 5.1l1.4 1.4M17.5 17.5l1.4 1.4M18.9 5.1l-1.4 1.4M6.5 17.5l-1.4 1.4" />
    </svg>
  );
}

export function CopyIcon(props: SVGProps<SVGSVGElement>) {
  return (
    <svg {...base} {...props}>
      <rect x="9" y="9" width="11" height="11" rx="2" />
      <path d="M5 15H4a1 1 0 0 1-1-1V4a1 1 0 0 1 1-1h10a1 1 0 0 1 1 1v1" />
    </svg>
  );
}

export function StopIcon(props: SVGProps<SVGSVGElement>) {
  return (
    <svg {...base} {...props}>
      <rect x="6" y="6" width="12" height="12" rx="2" fill="currentColor" stroke="none" />
    </svg>
  );
}

export function PlayIcon(props: SVGProps<SVGSVGElement>) {
  return (
    <svg {...base} {...props}>
      <path d="M7 5.5v13l11-6.5-11-6.5Z" fill="currentColor" stroke="none" />
    </svg>
  );
}

export function StatsIcon(props: SVGProps<SVGSVGElement>) {
  return (
    <svg {...base} {...props}>
      <path d="M5 20V10M12 20V4M19 20v-7" />
    </svg>
  );
}

export function EyeIcon(props: SVGProps<SVGSVGElement>) {
  // The R18 "N watching" glyph.
  return (
    <svg {...base} {...props}>
      <path d="M2.5 12S6 5.5 12 5.5 21.5 12 21.5 12 18 18.5 12 18.5 2.5 12 2.5 12Z" />
      <circle cx="12" cy="12" r="3" />
    </svg>
  );
}

// R15 (docs/20 Decision 9): the viewer's audio controls — rendered only when
// audio is actually received in the stream.
export function SpeakerIcon(props: SVGProps<SVGSVGElement>) {
  return (
    <svg {...base} {...props}>
      <path d="M4 9.5v5h3.5L12 18.5v-13L7.5 9.5H4Z" />
      <path d="M15.5 9a4 4 0 0 1 0 6" />
      <path d="M18 6.5a7.5 7.5 0 0 1 0 11" />
    </svg>
  );
}

export function SpeakerMutedIcon(props: SVGProps<SVGSVGElement>) {
  return (
    <svg {...base} {...props}>
      <path d="M4 9.5v5h3.5L12 18.5v-13L7.5 9.5H4Z" />
      <path d="M16 9.5l5 5M21 9.5l-5 5" />
    </svg>
  );
}

// The overflow ("kebab") glyph — the pointer-agnostic way into the viewer
// menu, which a right-click alone left out of reach on touch devices
// (docs/24 review finding PRODUCT-2). Dots are zero-length round-capped
// segments, so they inherit the file's stroke styling.
export function MoreIcon(props: SVGProps<SVGSVGElement>) {
  return (
    <svg {...base} {...props}>
      <path d="M12 5.5h.01M12 12h.01M12 18.5h.01" strokeWidth={2.4} />
    </svg>
  );
}

// Stacked rack units — the landing chip's mark. It replaces a bare filled
// circle, which read as a status LED the chip has no status to report.
export function PlusIcon(props: SVGProps<SVGSVGElement>) {
  return (
    <svg {...base} {...props}>
      <path d="M12 5v14M5 12h14" />
    </svg>
  );
}

export function ServerIcon(props: SVGProps<SVGSVGElement>) {
  return (
    <svg {...base} {...props}>
      <rect x="3.5" y="4" width="17" height="6.5" rx="1.6" />
      <rect x="3.5" y="13.5" width="17" height="6.5" rx="1.6" />
      <path d="M6.8 7.25h.01M6.8 16.75h.01" />
    </svg>
  );
}

export function CloseIcon(props: SVGProps<SVGSVGElement>) {
  return (
    <svg {...base} {...props}>
      <path d="M6 6l12 12M18 6L6 18" />
    </svg>
  );
}

// ── R42 (docs/44 §4.9): the room view's glyphs ──────────────────────────────

// Two people — the header's people-and-chat toggle, the broadcaster's Room
// button.
export function PeopleIcon(props: SVGProps<SVGSVGElement>) {
  return (
    <svg {...base} {...props}>
      <circle cx="9" cy="8" r="3.2" />
      <path d="M3.5 19.5v-1a5.5 5.5 0 0 1 11 0v1" />
      <path d="M15.5 5.5a3 3 0 0 1 0 5.6M17 13.2a5 5 0 0 1 3.5 4.8v1.5" />
    </svg>
  );
}

// Four cells — grid mode.
export function GridIcon(props: SVGProps<SVGSVGElement>) {
  return (
    <svg {...base} {...props}>
      <rect x="4" y="4" width="6.5" height="6.5" rx="1.2" />
      <rect x="13.5" y="4" width="6.5" height="6.5" rx="1.2" />
      <rect x="4" y="13.5" width="6.5" height="6.5" rx="1.2" />
      <rect x="13.5" y="13.5" width="6.5" height="6.5" rx="1.2" />
    </svg>
  );
}

// One large cell with a small one inset — focus mode.
export function FocusIcon(props: SVGProps<SVGSVGElement>) {
  return (
    <svg {...base} {...props}>
      <rect x="4" y="5" width="16" height="14" rx="1.6" />
      <rect x="12.5" y="8" width="5" height="4" rx="0.8" fill="currentColor" stroke="none" />
    </svg>
  );
}

// A struck-through cell — hide videos.
export function VideoOffIcon(props: SVGProps<SVGSVGElement>) {
  return (
    <svg {...base} {...props}>
      <path d="M4 7.5a1.5 1.5 0 0 1 1.5-1.5H14a1.5 1.5 0 0 1 1.5 1.5v9A1.5 1.5 0 0 1 14 18H5.5A1.5 1.5 0 0 1 4 16.5v-9Z" />
      <path d="M15.5 10.5 20 8v8l-4.5-2.5" />
      <path d="M3 3l18 18" />
    </svg>
  );
}

// A pencil — edit the nickname.
export function EditIcon(props: SVGProps<SVGSVGElement>) {
  return (
    <svg {...base} {...props}>
      <path d="M4 20h4l10.5-10.5a2 2 0 0 0 0-2.8l-1.2-1.2a2 2 0 0 0-2.8 0L4 16v4Z" />
      <path d="M13 7l4 4" />
    </svg>
  );
}

// An outward arrow in a frame — open this POV full-screen (a #/view/ link).
export function OpenIcon(props: SVGProps<SVGSVGElement>) {
  return (
    <svg {...base} {...props}>
      <path d="M14 4h6v6M20 4l-8 8" />
      <path d="M19 14v4.5A1.5 1.5 0 0 1 17.5 20h-12A1.5 1.5 0 0 1 4 18.5v-12A1.5 1.5 0 0 1 5.5 5H10" />
    </svg>
  );
}

// A pin — keep the people-and-chat panel open while the chrome fades.
export function PinIcon(props: SVGProps<SVGSVGElement>) {
  return (
    <svg {...base} {...props}>
      <path d="M9 4h6l-1 6 3 3v1.5H7V13l3-3-1-6Z" />
      <path d="M12 14.5V21" />
    </svg>
  );
}

// A monitor — the broadcaster's "change source" on their own tile.
export function ScreenIcon(props: SVGProps<SVGSVGElement>) {
  return (
    <svg {...base} {...props}>
      <rect x="3" y="5" width="18" height="12" rx="1.6" />
      <path d="M9 20h6M12 17v3" />
    </svg>
  );
}
