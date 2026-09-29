# Console browser regression tests

The suite runs against `github`, `stripe`, and `neumorphism` Playwright projects.
GitHub-specific checks validate Primer Light/Dark surfaces, restrained controls,
the green primary action, blue focus rings, and reduced motion. Stripe-specific
checks validate the adapted StyleKit colors, grid, layered shadows, button
hover/press, focus and reduced motion. Representative text,
links, placeholders, statuses and log text are checked against their computed
opaque background for 4.5:1 contrast (not a complete accessibility audit).
Resource, workflow and Job views are checked at 390, 768 and 1600px widths;
wide data is allowed to scroll inside its own container, not the page.
Use `npm run test:browser -- --project=github` to run only the default style.

From `console/frontend`:

```sh
npm ci
npx playwright install --with-deps chromium
npm run test:browser
```

The tests run the production frontend bundle in Chromium, with mocked API
responses and the Console SPA document fallback. They do not require a
Kubernetes cluster or replace the cluster E2E suite.

Coverage includes individual Job dependency edges, shared predecessors,
edge alignment after zoom and resize, and Tailwind utility precedence over
global element defaults. Theme/interaction tests also exercise Light, Dark,
System, keyboard focus, reduced motion, pressed buttons, Job navigation, and
automatic Step log loading. Resource-page tests cover Home, About, Run and
Runtime lists/details, and the WorkflowRun list. Screenshots are saved under
the Playwright output directory for visual review, not used as pixel baselines.
Installing browser system dependencies may require
administrator privileges on Linux.

## Adding a visual style

Add a CSS file under `src/styles/`, import it from `src/appearance.ts`, and add
its ID and label to the `styles` registry there. Define light variables on
`:root[data-style="your-id"]` and dark overrides with
`[data-color-scheme="dark"]`. Define the complete semantic token contract:
surface/field colors, text/status colors, borders, panel/control radii, and
panel/button/field/selected/hover/log shadows and backgrounds. Keep each style's
visual rules in its own CSS file; `github.css`, `stripe.css`, and
`neumorphism.css` are the current examples.
Keep layout and interaction in the shared components; do not branch page JSX
on the selected style. Add the style to the browser-test projects and visual
expectations, then inspect its light/dark screenshots and keyboard/mobile behavior.

Style uses `kruntimes-console-style`; Theme uses the
`kruntimes-console-theme` key. Preferences are initialized before React mounts;
storage failures fall back safely. No server or Helm configuration is involved.
