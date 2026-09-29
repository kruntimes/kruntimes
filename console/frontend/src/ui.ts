// Shared Tailwind recipes keep JSX focused on page structure rather than a
// repeated sequence of low-level spacing, border, and color utilities.
export const ui = {
  primaryButton:
    "ui-button ui-primary inline-flex items-center justify-center px-4 py-2 text-sm no-underline",
  panel: "ui-panel",
  subtlePanel: "ui-field ui-canvas",
  button:
    "ui-button inline-flex items-center justify-center px-4 py-2 text-sm no-underline",
  iconButton: "ui-button inline-grid size-10 place-items-center p-0",
  searchField:
    "ui-field flex min-w-56 items-center gap-2 px-3 py-2 text-sm text-[var(--muted)] focus-within:outline-2 focus-within:outline-offset-2 focus-within:outline-[var(--link)]",
  textInput:
    "w-full border-0 bg-transparent text-[var(--text)] outline-none placeholder:text-[var(--muted)]",
  navItem:
    "ui-nav flex items-center gap-2 px-3 py-3 text-[var(--text)] no-underline",
  selectedNavItem: "ui-selected font-semibold text-[var(--link)]",
} as const;
