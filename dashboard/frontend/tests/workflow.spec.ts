import { expect, test, type Page } from "@playwright/test";
import { readFile } from "node:fs/promises";
import { expectTextContrast } from "./contrast";

type WorkflowStepFixture = {
  name: string;
  phase: string;
  runName?: string;
  actionSteps?: Array<{ name: string; phase: string; runName?: string }>;
};

test("fresh browser defaults and invalid theme are safe", async ({ page }) => {
  await page.addInitScript(() => {
    localStorage.removeItem("kruntimes-dashboard-style");
    localStorage.setItem("kruntimes-dashboard-theme", "invalid");
    new MutationObserver(() => {
      if (
        document.getElementById("root")?.childElementCount &&
        !document.documentElement.dataset.firstStyle
      ) {
        document.documentElement.dataset.firstStyle =
          document.documentElement.dataset.style;
      }
    }).observe(document, { childList: true, subtree: true });
  });
  await openWorkflow(page);
  await expect(page.locator("html")).toHaveAttribute("data-style", "github");
  await expect(page.locator("html")).toHaveAttribute("data-theme", "system");
  await expect(page.locator("html")).toHaveAttribute(
    "data-first-style",
    "github",
  );
});

async function openWorkflow(
  page: Page,
  sharedDependencies = false,
  steps: WorkflowStepFixture[] = [
    { name: "Build", phase: "Succeeded", runName: "build-run" },
  ],
) {
  // The Go server serves index.html for frontend routes; Vite preview only
  // supports the configured /assets/ base. Reproduce that SPA fallback here.
  await page.route(
    "**/namespaces/default/workflowruns/review**",
    async (route) => {
      if (route.request().resourceType() !== "document")
        return route.fallback();
      await route.fulfill({
        contentType: "text/html",
        body: await readFile("../backend/assets/index.html"),
      });
    },
  );
  await page.route("**/settings", async (route) => {
    if (route.request().resourceType() !== "document") return route.fallback();
    await route.fulfill({
      contentType: "text/html",
      body: await readFile("../backend/assets/index.html"),
    });
  });
  await page.route("**/api/**", async (route) => {
    const path = new URL(route.request().url()).pathname;
    const json = path.endsWith("/session")
      ? { authenticated: true }
      : path === "/api/namespaces"
        ? { items: ["default"] }
        : {
            name: "review",
            namespace: "default",
            phase: "Succeeded",
            creationTimestamp: "2026-09-01T00:00:00Z",
            spec: {
              jobs: {
                A: {},
                B: {},
                C: { needs: sharedDependencies ? ["A", "B"] : ["A"] },
                D: { needs: sharedDependencies ? ["A", "B"] : ["B"] },
              },
            },
            status: {
              jobs: {
                A: {
                  phase: "Succeeded",
                  steps,
                },
                B: { phase: "Succeeded" },
                C: { phase: "Succeeded" },
                D: { phase: "Succeeded" },
              },
            },
          };
    await route.fulfill({ json });
  });
  await page.goto("/namespaces/default/workflowruns/review");
}

async function changeAppearance(
  page: Page,
  appearance: { style?: string; theme?: string },
) {
  const returnURL = page.url();
  await page.goto("/settings");
  if (appearance.style) {
    await page
      .getByRole("combobox", { name: "Style", exact: true })
      .selectOption(appearance.style);
  }
  if (appearance.theme) {
    await page
      .getByRole("combobox", { name: "Theme", exact: true })
      .selectOption(appearance.theme);
  }
  await page.goto(returnURL);
}

// Compare the rendered SVG endpoints with the visible Job rows, in screen
// coordinates. This catches both lost dependencies and double-applied zoom.
async function renderedDependencies(page: Page) {
  return page.locator(".dag").evaluate((canvas) => {
    const nodes = [...canvas.querySelectorAll<HTMLAnchorElement>(".dag-node")];
    return [...canvas.querySelectorAll<SVGPathElement>(".dag-edges path")]
      .map((edge) => {
        const matrix = edge.getScreenCTM()!;
        const start = edge.getPointAtLength(0).matrixTransform(matrix);
        const end = edge
          .getPointAtLength(edge.getTotalLength())
          .matrixTransform(matrix);
        const find = (point: DOMPoint, side: "left" | "right") => {
          const node = nodes.find((item) => {
            const rect = item.getBoundingClientRect();
            return (
              Math.abs(rect[side] - point.x) < 1 &&
              Math.abs(rect.top + rect.height / 2 - point.y) < 1
            );
          });
          return node?.querySelector("strong")?.textContent ?? "disconnected";
        };
        return find(start, "right") + "->" + find(end, "left");
      })
      .sort();
  });
}

test("switching style from Settings preserves dependency edges", async ({
  page,
}, testInfo) => {
  await openWorkflow(page);
  await changeAppearance(page, {
    style: testInfo.project.name === "github" ? "neumorphism" : "github",
  });
  await expect.poll(() => renderedDependencies(page)).toEqual(["A->C", "B->D"]);
});

test("appearance persists independently and survives invalid preferences", async ({
  page,
}) => {
  await page.addInitScript(() => {
    if (!sessionStorage.getItem("initialized")) {
      localStorage.setItem("kruntimes-dashboard-style", "unknown");
      localStorage.setItem("kruntimes-dashboard-theme", "dark");
      sessionStorage.setItem("initialized", "yes");
    }
  });
  await openWorkflow(page);
  await expect(page.locator("html")).toHaveAttribute("data-style", "github");
  await expect(page.locator("html")).toHaveAttribute("data-theme", "dark");
  await changeAppearance(page, { style: "neumorphism" });
  await page.reload();
  await expect(page.locator("html")).toHaveAttribute(
    "data-style",
    "neumorphism",
  );
  await expect(page.locator("html")).toHaveAttribute("data-theme", "dark");
  await changeAppearance(page, { theme: "light" });
  await expect(page.locator("html")).toHaveAttribute(
    "data-style",
    "neumorphism",
  );
});

test("unavailable storage uses defaults without breaking switching", async ({
  page,
}) => {
  const errors: string[] = [];
  page.on("pageerror", (error) => errors.push(error.message));
  await page.addInitScript(() => {
    Object.defineProperty(window, "localStorage", {
      get() {
        throw new DOMException("Blocked", "SecurityError");
      },
    });
  });
  await openWorkflow(page);
  await expect(page.locator("html")).toHaveAttribute("data-style", "github");
  await expect(page.locator("html")).toHaveAttribute("data-theme", "system");
  await changeAppearance(page, { style: "neumorphism", theme: "dark" });
  await expect(page.locator("html")).toHaveAttribute(
    "data-style",
    "neumorphism",
  );
  await expect(page.locator("html")).toHaveAttribute(
    "data-color-scheme",
    "dark",
  );
  expect(errors).toEqual([]);
});

test("parallel jobs retain distinct dependency relationships", async ({
  page,
}) => {
  await openWorkflow(page);
  await expect.poll(() => renderedDependencies(page)).toEqual(["A->C", "B->D"]);
});

test("shared predecessors retain all four dependency edges", async ({
  page,
}) => {
  await openWorkflow(page, true);
  await expect
    .poll(() => renderedDependencies(page))
    .toEqual(["A->C", "A->D", "B->C", "B->D"]);
});

test("edges stay attached after zoom and resize", async ({ page }) => {
  const errors: string[] = [];
  page.on("pageerror", (error) => errors.push(error.message));
  await openWorkflow(page);
  for (const zoom of ["Zoom out", "Zoom in"]) {
    await page.setViewportSize({ width: 1600, height: 1000 });
    await page
      .locator(".dag-node strong")
      .first()
      .evaluate((node) => {
        (node as HTMLElement).style.paddingBottom = "0px";
      });
    await page.getByRole("button", { name: zoom, exact: true }).click();
    await expect
      .poll(() => renderedDependencies(page))
      .toEqual(["A->C", "B->D"]);
    await page.setViewportSize({ width: 1100, height: 800 });
    // Force a node resize as well as a viewport resize at non-default zoom.
    await page
      .locator(".dag-node strong")
      .first()
      .evaluate((node) => {
        (node as HTMLElement).style.paddingBottom = "24px";
      });
    await expect
      .poll(() => renderedDependencies(page))
      .toEqual(["A->C", "B->D"]);
    await page.getByRole("button", { name: "Fit", exact: true }).click();
  }
  expect(errors).toEqual([]);
});

test("DAG canvas pans by dragging without stealing Job clicks", async ({
  page,
}) => {
  await openWorkflow(page);
  await page.setViewportSize({ width: 390, height: 844 });
  const scroll = page.locator(".dag-scroll");
  await scroll.evaluate((node) => {
    node.style.height = "240px";
    node.querySelector<HTMLElement>(".dag")!.style.paddingBottom = "700px";
  });
  const box = await scroll.boundingBox();
  if (!box) throw new Error("DAG viewport is not visible");
  await page.mouse.move(box.x + 20, box.y + 200);
  await page.mouse.down();
  await expect(scroll).toHaveAttribute("data-dag-dragging", "true");
  await page.mouse.move(box.x - 120, box.y + 80);
  await page.mouse.up();
  await expect(scroll).not.toHaveAttribute("data-dag-dragging", "true");
  await expect
    .poll(() => scroll.evaluate((node) => node.scrollLeft))
    .toBeGreaterThan(0);
  await expect
    .poll(() => scroll.evaluate((node) => node.scrollTop))
    .toBeGreaterThan(0);
  await page.locator(".dag-node").filter({ hasText: "A" }).click();
  await expect(page).toHaveURL(/\/jobs\/A$/);
});

test("Tailwind utilities override global element defaults", async ({
  page,
}) => {
  await openWorkflow(page);
  await expect(page.locator(".dag-scroll")).toHaveCSS("font-size", "14px");
  await expect(page.locator(".dag-stage").first()).toHaveCSS("width", "300px");
  await expect(
    page
      .getByRole("navigation", { name: "Workflow jobs" })
      .locator("a")
      .first(),
  ).toHaveCSS("font-size", "14px");
  await expect(
    page.getByRole("button", { name: "Zoom in", exact: true }),
  ).toHaveCSS("width", "40px");
  await expect(page.getByLabel("Search jobs").locator("..")).toHaveCSS(
    "display",
    "flex",
  );
  await expect(page.locator("dl")).toHaveCSS("display", "flex");
  await expect(page.locator("main")).toHaveCSS("padding", "0px");
  await page.setViewportSize({ width: 700, height: 800 });
  await expect(page.locator("main")).toHaveCSS("padding", "0px");
});

test("themes, focus and pressed controls", async ({ page }, testInfo) => {
  await openWorkflow(page);
  const neumorphic = testInfo.project.name === "neumorphism";
  const github = testInfo.project.name === "github";
  const depth = neumorphic ? /inset/ : github ? /1px/ : "none";
  const zoom = page.getByRole("button", { name: "Zoom in", exact: true });
  for (const mode of ["light", "dark"]) {
    await changeAppearance(page, { theme: mode });
    await expect(page.locator("html")).toHaveAttribute("data-theme", mode);
    await expect(zoom).toHaveCSS(
      "color",
      neumorphic
        ? mode === "light"
          ? "rgb(36, 52, 72)"
          : "rgb(237, 242, 247)"
        : github
          ? mode === "light"
            ? "rgb(31, 35, 40)"
            : "rgb(201, 209, 217)"
          : mode === "light"
            ? "rgb(10, 37, 64)"
            : "rgb(237, 240, 247)",
    );
    await expect(page.locator(".dag-stage").first()).not.toHaveCSS(
      "box-shadow",
      "none",
    );
    await expect(page.getByLabel("Search jobs").locator("..")).toHaveCSS(
      "box-shadow",
      neumorphic ? /inset/ : github ? "none" : /rgba\(0, 0, 0, 0.05\)/,
    );
    await expect(
      page.getByRole("button", { name: "Pipeline", exact: true }),
    ).toHaveCSS("box-shadow", depth);
    await page.getByRole("button", { name: "Zoom out", exact: true }).focus();
    await page.keyboard.press("Tab");
    await expect(zoom).toBeFocused();
    await expect(zoom).toHaveCSS("outline-style", "solid");
    await zoom.hover();
    await page.mouse.down();
    await expect(zoom).toHaveCSS("box-shadow", github ? "none" : /inset/);
    await page.mouse.up();
    await page.getByRole("button", { name: "Fit", exact: true }).click();
    await page.screenshot({
      path: testInfo.outputPath(`workflow-${mode}.png`),
      fullPage: true,
      animations: "disabled",
    });
    if (!neumorphic) await expectTextContrast(page);
    for (const width of [390, 768, 1600]) {
      await page.setViewportSize({ width, height: 1000 });
      expect(
        await page.evaluate(
          () => document.documentElement.scrollWidth <= innerWidth,
        ),
      ).toBe(true);
      await page.screenshot({
        path: testInfo.outputPath(`workflow-${mode}-${width}.png`),
        fullPage: true,
        animations: "disabled",
      });
    }
  }
  await changeAppearance(page, { theme: "system" });
  for (const mode of ["light", "dark"] as const) {
    await page.emulateMedia({ colorScheme: mode });
    await expect(page.locator("body")).toHaveCSS(
      "background-color",
      neumorphic
        ? mode === "light"
          ? "rgb(230, 235, 240)"
          : "rgb(39, 47, 59)"
        : github
          ? mode === "light"
            ? "rgb(246, 248, 250)"
            : "rgb(13, 17, 23)"
          : mode === "light"
            ? "rgb(246, 249, 252)"
            : "rgb(21, 26, 37)",
    );
  }
  await page.emulateMedia({ reducedMotion: "reduce" });
  await expect(zoom).toHaveCSS("transition-duration", "0s");
});

test("Job navigation and expanding a recessed Step loads logs", async ({
  page,
}, testInfo) => {
  const depth =
    testInfo.project.name === "neumorphism"
      ? /inset/
      : testInfo.project.name === "github"
        ? "none"
        : /rgba\(0, 0, 0, 0.05\)/;
  let logRequests = 0;
  await openWorkflow(page);
  await page.route("**/runs/build-run/logs?*", async (route) => {
    logRequests++;
    await route.fulfill({
      json: { items: [{ stream: "stdout", message: "Build complete" }] },
    });
  });
  await page
    .locator(".dag-node")
    .filter({ has: page.locator("strong", { hasText: /^A$/ }) })
    .click();
  await expect(page).toHaveURL(/\/jobs\/A$/);
  await expect(page.locator(".step")).toBeVisible();
  expect(logRequests).toBe(0);
  await page.locator(".step summary").click();
  await expect(page.locator(".log-viewer")).toContainText("Build complete");
  await expect(page.locator(".step")).toHaveCSS("box-shadow", depth);
  expect(logRequests).toBe(1);
  for (const mode of ["light", "dark"]) {
    await changeAppearance(page, { theme: mode });
    await page.screenshot({
      path: testInfo.outputPath(`job-${mode}.png`),
      fullPage: true,
      animations: "disabled",
    });
    if (["stripe", "github"].includes(testInfo.project.name)) {
      await expectTextContrast(page);
    }
    for (const width of [390, 768, 1600]) {
      await page.setViewportSize({ width, height: 1000 });
      expect(
        await page.evaluate(
          () => document.documentElement.scrollWidth <= innerWidth,
        ),
      ).toBe(true);
      await page.screenshot({
        path: testInfo.outputPath(`job-${mode}-${width}.png`),
        fullPage: true,
        animations: "disabled",
      });
    }
  }
});

test("expanding Action and command steps loads each step's Run logs", async ({
  page,
}) => {
  const logs = {
    "checkout-run": "Checked out main",
    "setup-run": "Go is ready",
    "test-run": "Tests passed",
  };
  await openWorkflow(page, false, [
    {
      name: "Checkout",
      phase: "Succeeded",
      actionSteps: [
        { name: "checkout", phase: "Succeeded", runName: "checkout-run" },
      ],
    },
    {
      name: "Setup Go",
      phase: "Succeeded",
      actionSteps: [
        { name: "install", phase: "Succeeded", runName: "setup-run" },
      ],
    },
    { name: "Test", phase: "Succeeded", runName: "test-run" },
  ]);
  await page.route("**/runs/*/logs?*", async (route) => {
    const runName = new URL(route.request().url()).pathname.split("/").at(-2)!;
    await route.fulfill({
      json: {
        items: [
          { stream: "stdout", message: logs[runName as keyof typeof logs] },
        ],
      },
    });
  });
  await page
    .locator(".dag-node")
    .filter({ has: page.locator("strong", { hasText: /^A$/ }) })
    .click();

  for (const [index, message] of Object.values(logs).entries()) {
    const step = page.locator(".step").nth(index);
    await step.locator("summary").click();
    await expect(step.locator(".log-viewer")).toContainText(message);
  }
});
