type SidebarIconName =
  "runs" | "runtimes" | "workflowruns" | "settings" | "about";

export function SidebarIcon({ name }: { name: SidebarIconName }) {
  const paths = {
    runs: (
      <>
        <rect x="3" y="4" width="18" height="16" rx="2" />
        <path d="m8 9 3 3-3 3M13 15h3" />
      </>
    ),
    runtimes: (
      <>
        <rect x="3" y="4" width="18" height="6" rx="1" />
        <rect x="3" y="14" width="18" height="6" rx="1" />
        <path d="M7 7h.01M7 17h.01M11 7h6M11 17h6" />
      </>
    ),
    workflowruns: (
      <>
        <circle cx="6" cy="5" r="2" />
        <circle cx="18" cy="7" r="2" />
        <circle cx="12" cy="19" r="2" />
        <path d="m7.8 6.1 8.3.8M7.2 6.8l3.7 10.3M16.8 8.8l-3.6 8.3" />
      </>
    ),
    settings: (
      <>
        <circle cx="12" cy="12" r="3" />
        <path d="M19.4 15a1.7 1.7 0 0 0 .3 1.9l.1.1-2.1 2.1-.1-.1a1.7 1.7 0 0 0-1.9-.3 1.7 1.7 0 0 0-1 1.6v.2h-3v-.2a1.7 1.7 0 0 0-1-1.6 1.7 1.7 0 0 0-1.9.3l-.1.1L6.6 17l.1-.1A1.7 1.7 0 0 0 7 15a1.7 1.7 0 0 0-1.6-1H5.2v-3h.2a1.7 1.7 0 0 0 1.6-1 1.7 1.7 0 0 0-.3-1.9L6.6 8l2.1-2.1.1.1a1.7 1.7 0 0 0 1.9.3 1.7 1.7 0 0 0 1-1.6v-.2h3v.2a1.7 1.7 0 0 0 1 1.6 1.7 1.7 0 0 0 1.9-.3l.1-.1L19.8 8l-.1.1a1.7 1.7 0 0 0-.3 1.9 1.7 1.7 0 0 0 1.6 1h.2v3H21a1.7 1.7 0 0 0-1.6 1Z" />
      </>
    ),
    about: (
      <>
        <circle cx="12" cy="12" r="9" />
        <path d="M12 11v5M12 8h.01" />
      </>
    ),
  } satisfies Record<SidebarIconName, React.ReactNode>;
  return (
    <span className="sidebar-icon-shell" aria-hidden="true">
      <svg
        className="sidebar-icon"
        data-icon={name}
        fill="none"
        focusable="false"
        stroke="currentColor"
        strokeLinecap="round"
        strokeLinejoin="round"
        strokeWidth="1.75"
        viewBox="0 0 24 24"
      >
        {paths[name]}
      </svg>
    </span>
  );
}
