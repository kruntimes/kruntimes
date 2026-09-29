import { expect, test } from "@playwright/test";
import { readFile } from "node:fs/promises";
import { expectTextContrast } from "./contrast";

test("GitHub tokens, restrained controls, focus, and dark Primer palette", async ({
  page,
}, info) => {
  test.skip(info.project.name !== "github", "GitHub-specific contract");
  await page.route("**/*", async (route) => {
    if (route.request().resourceType() === "document") {
      return route.fulfill({
        contentType: "text/html",
        body: await readFile("../backend/assets/index.html"),
      });
    }
    const path = new URL(route.request().url()).pathname;
    if (!path.startsWith("/api/")) return route.continue();
    return route.fulfill({
      json:
        path === "/api/namespaces"
          ? { items: ["default"] }
          : { authenticated: false },
    });
  });
  await page.goto("/");

  await expect(page.locator("html")).toHaveAttribute("data-style", "github");
  await expect(page.locator("body")).toHaveCSS("background-image", "none");
  for (const [mode, colors] of Object.entries({
    light: {
      background: "rgb(246, 248, 250)",
      panel: "rgb(255, 255, 255)",
      button: "rgb(31, 136, 61)",
    },
    dark: {
      background: "rgb(13, 17, 23)",
      panel: "rgb(22, 27, 34)",
      button: "rgb(35, 134, 54)",
    },
  })) {
    await page.goto("/settings");
    await page
      .getByRole("combobox", { name: "Theme", exact: true })
      .selectOption(mode);
    await page.goto("/");
    await page.getByRole("button", { name: "Log in", exact: true }).click();
    const dialog = page.getByRole("dialog", { name: "Log in" });
    const button = dialog.getByRole("button", { name: "Log in", exact: true });
    const panel = dialog.locator("section");
    const textarea = dialog.locator("textarea");
    await expect(page.locator("body")).toHaveCSS(
      "background-color",
      colors.background,
    );
    await expect(panel).toHaveCSS("background-color", colors.panel);
    await expect(panel).toHaveCSS("border-radius", "6px");
    await expect(panel).toHaveCSS("box-shadow", /1px/);
    await expect(button).toHaveCSS("background-color", colors.button);
    await expect(button).toHaveCSS("color", "rgb(255, 255, 255)");
    await expect(button).toHaveCSS("border-radius", "6px");
    await button.hover();
    await expect(button).toHaveCSS("transform", "none");
    await page.mouse.down();
    await expect(button).toHaveCSS(
      "transform",
      "matrix(0.98, 0, 0, 0.98, 0, 0)",
    );
    await page.mouse.up();
    await textarea.focus();
    await expect(textarea).toHaveCSS("box-shadow", /rgb\(9, 105, 218\).*3px/);
    await expectTextContrast(page);
  }
  await page.emulateMedia({ reducedMotion: "reduce" });
  const button = page
    .getByRole("dialog", { name: "Log in" })
    .getByRole("button", { name: "Log in", exact: true });
  await button.hover();
  await expect(button).toHaveCSS("transition-duration", "0s");
  await expect(button).toHaveCSS("transform", "none");
});
