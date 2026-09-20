import { expect, test } from "@playwright/test";
import { readFile } from "node:fs/promises";
import { expectTextContrast } from "./contrast";

test("resource pages share raised surfaces in both themes", async ({
  page,
}, testInfo) => {
  const style = testInfo.project.name;
  const panelBackground = {
    github: {
      light: "rgb(255, 255, 255)",
      dark: "rgb(22, 27, 34)",
    },
    stripe: {
      light: "rgb(255, 255, 255)",
      dark: "rgb(32, 39, 55)",
    },
    neumorphism: {
      light: "rgb(230, 235, 240)",
      dark: "rgb(39, 47, 59)",
    },
  } as const;
  const phaseBackgrounds = {
    github: {
      light: {
        Succeeded: "rgb(218, 251, 225)",
        Failed: "rgb(255, 235, 233)",
        Running: "rgb(221, 244, 255)",
        Pending: "rgb(246, 248, 250)",
      },
      dark: {
        Succeeded: "rgb(13, 59, 28)",
        Failed: "rgb(75, 11, 16)",
        Running: "rgb(12, 45, 107)",
        Pending: "rgb(33, 38, 45)",
      },
    },
    stripe: {
      light: {
        Succeeded: "rgb(231, 245, 237)",
        Failed: "rgb(251, 236, 239)",
        Running: "rgb(232, 243, 252)",
        Pending: "rgb(241, 244, 248)",
      },
      dark: {
        Succeeded: "rgb(31, 70, 53)",
        Failed: "rgb(82, 44, 58)",
        Running: "rgb(24, 61, 85)",
        Pending: "rgb(43, 52, 72)",
      },
    },
    neumorphism: {
      light: {
        Succeeded: "rgb(220, 238, 227)",
        Failed: "rgb(242, 221, 226)",
        Running: "rgb(220, 236, 241)",
        Pending: "rgb(230, 235, 240)",
      },
      dark: {
        Succeeded: "rgb(41, 77, 59)",
        Failed: "rgb(84, 47, 61)",
        Running: "rgb(40, 73, 86)",
        Pending: "rgb(48, 58, 72)",
      },
    },
  } as const;
  const errors: string[] = [];
  page.on("pageerror", (error) => errors.push(error.message));
  const run = {
    name: "build",
    namespace: "default",
    uid: "build-1",
    runtime: "bash",
    mode: "Task",
    phase: "Succeeded",
    creationTimestamp: "2026-09-01T00:00:00Z",
    assignedPod: "bash-1",
    spec: { runtime: "bash" },
    status: { phase: "Succeeded" },
  };
  const runtime = {
    name: "bash",
    namespace: "default",
    replicas: 1,
    readyReplicas: 1,
    capacity: { executions: "10" },
    runCount: 1,
    healthy: true,
  };
  const workflow = {
    name: "release",
    uid: "release-1",
    namespace: "default",
    phase: "Succeeded",
    jobCount: 4,
    creationTimestamp: run.creationTimestamp,
  };
  await page.route("**/*", async (route) => {
    if (route.request().resourceType() === "document") {
      return route.fulfill({
        contentType: "text/html",
        body: await readFile("../backend/assets/index.html"),
      });
    }
    const path = new URL(route.request().url()).pathname;
    if (!path.startsWith("/api/")) return route.continue();
    const data: Record<string, unknown> = {
      "/api/session": { authenticated: true },
      "/api/namespaces": { items: ["default"] },
      "/api/namespaces/default/runs": {
        items: [
          run,
          ...["Failed", "Running", "Cancelled", "Pending"].map((phase) => ({
            ...run,
            name: phase.toLowerCase(),
            uid: phase,
            phase,
          })),
        ],
      },
      "/api/namespaces/default/runs/build": run,
      "/api/namespaces/default/runtimes": { items: [runtime] },
      "/api/namespaces/default/runtimes/bash": {
        runtime,
        spec: {},
        status: {},
        pods: [
          {
            name: "bash-1",
            phase: "Running",
            ready: true,
            runtimedReady: true,
            runs: [run],
          },
        ],
      },
      "/api/namespaces/default/workflowruns": { items: [workflow] },
    };
    await route.fulfill({ json: data[path] ?? { authenticated: true } });
  });
  const paths = [
    "/",
    "/settings",
    "/about",
    "/namespaces/default/runs",
    "/namespaces/default/runs/build",
    "/namespaces/default/runtimes",
    "/namespaces/default/runtimes/bash",
    "/namespaces/default/workflowruns",
  ];
  for (const mode of ["light", "dark"]) {
    for (const [index, path] of paths.entries()) {
      await page.goto("/settings");
      await page
        .getByRole("combobox", { name: "Theme", exact: true })
        .selectOption(mode);
      await page.goto(path);
      const panel = page.locator("main .ui-panel").first();
      await expect(panel).toBeVisible();
      await expect(panel).not.toHaveCSS("box-shadow", "none");
      await expect(panel).toHaveCSS(
        "background-color",
        panelBackground[style as keyof typeof panelBackground][
          mode as "light" | "dark"
        ],
      );
      if (path.endsWith("/runs")) {
        await expect(page.locator(".phase")).toHaveCount(5);
        for (const [phase, color] of Object.entries(
          phaseBackgrounds[style as keyof typeof phaseBackgrounds][
            mode as "light" | "dark"
          ],
        ))
          await expect(page.locator(`.phase.${phase}`)).toHaveCSS(
            "background-color",
            color,
          );
        await expect(page.locator("aside .ui-selected")).toContainText("Runs");
      }
      await expect(page.locator("aside .sidebar-icon")).toHaveCount(5);
      await expect(
        page.getByRole("navigation", { name: "Kruntimes resources" }),
      ).toContainText("Runs");
      await expect(
        page.getByRole("navigation", { name: "Kruntimes resources" }),
      ).toContainText("Runtimes");
      await expect(
        page.getByRole("navigation", { name: "Dashboard" }),
      ).toContainText("Settings");
      await expect(
        page.getByRole("navigation", { name: "Dashboard" }),
      ).toContainText("About");
      await expect(page.locator("aside [data-icon='runs']")).toBeVisible();
      await expect(page.locator("aside [data-icon='runtimes']")).toBeVisible();
      await expect(
        page.locator("aside [data-icon='workflowruns']"),
      ).toBeVisible();
      await expect(page.locator("aside [data-icon='settings']")).toBeVisible();
      await expect(page.locator("aside [data-icon='about']")).toBeVisible();
      if (path === "/settings") {
        await expect(page.locator("aside .ui-selected")).toContainText(
          "Settings",
        );
        await expect(
          page.getByRole("combobox", { name: "Style", exact: true }),
        ).toHaveValue(style);
      }
      if (
        path.endsWith("/runtimes/bash") &&
        ["stripe", "github"].includes(testInfo.project.name)
      ) {
        await expect(page.locator(".ui-embedded")).toHaveCSS(
          "box-shadow",
          "none",
        );
        await expect(page.locator(".ui-embedded")).toHaveCSS(
          "border-radius",
          "0px",
        );
      }
      await page.screenshot({
        path: testInfo.outputPath(`resource-${index}-${mode}.png`),
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
      }
    }
  }
  await page.setViewportSize({ width: 390, height: 844 });
  await expect(
    page.getByRole("combobox", { name: "Namespace", exact: true }),
  ).toBeVisible();
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= innerWidth,
    ),
  ).toBe(true);
  expect(errors).toEqual([]);
});
