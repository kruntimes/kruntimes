import { expect, test } from "@playwright/test";
import { readFile } from "node:fs/promises";
import { expectTextContrast } from "./contrast";

test("Stripe tokens, button motion, focus and contrast", async ({
  page,
}, info) => {
  test.skip(info.project.name !== "stripe", "Stripe-specific contract");
  await page.route("**/*", async (route) => {
    if (route.request().resourceType() === "document")
      return route.fulfill({
        contentType: "text/html",
        body: await readFile("../backend/assets/index.html"),
      });
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
  for (const mode of ["light", "dark"]) {
    await page.goto("/settings");
    await page
      .getByRole("combobox", { name: "Theme", exact: true })
      .selectOption(mode);
    await page.goto("/");
    await page.getByRole("button", { name: "Log in", exact: true }).click();
    const dialog = page.getByRole("dialog", { name: "Log in" });
    const button = dialog.getByRole("button", { name: "Log in", exact: true });
    const panel = dialog.locator("section");
    await expect(button).toHaveCSS("background-color", "rgb(99, 91, 255)");
    await expect(button).toHaveCSS("color", "rgb(255, 255, 255)");
    await expect(button).toHaveCSS("padding", "12px 24px");
    await expect(button).toHaveCSS("border-radius", "8px");
    await expect(panel).toHaveCSS("border-radius", "12px");
    await expect(panel).toHaveCSS("box-shadow", /4px.*16px/);
    await expect(page.locator("body")).toHaveCSS(
      "background-size",
      "40px 40px, 40px 40px",
    );
    await expect(page.locator("body")).toHaveCSS(
      "background-image",
      /linear-gradient.*linear-gradient/,
    );
    await button.hover();
    await expect(button).toHaveCSS("transform", "matrix(1, 0, 0, 1, 0, -2)");
    await expect(button).toHaveCSS("box-shadow", /inset/);
    await page.mouse.down();
    await expect(button).toHaveCSS(
      "transform",
      "matrix(0.98, 0, 0, 0.98, 0, 0)",
    );
    await expect(button).toHaveCSS(
      "box-shadow",
      /^rgba\(0, 0, 0, 0.2\).*inset$/,
    );
    await page.mouse.move(0, 0);
    await page.mouse.up();
    await page.locator("textarea").focus();
    await expect(page.locator("textarea")).toHaveCSS(
      "box-shadow",
      /rgb\(99, 91, 255\).*2px/,
    );
    await expectTextContrast(page);
    for (const width of [390, 768, 1600]) {
      await page.setViewportSize({ width, height: 1000 });
      expect(
        await page.evaluate(
          () => document.documentElement.scrollWidth <= innerWidth,
        ),
      ).toBe(true);
      await page.screenshot({
        path: info.outputPath(`${mode}-${width}.png`),
        fullPage: true,
        animations: "disabled",
      });
    }
  }
  await page.emulateMedia({ reducedMotion: "reduce" });
  const button = page
    .getByRole("dialog", { name: "Log in" })
    .getByRole("button", { name: "Log in", exact: true });
  await button.hover();
  await expect(button).toHaveCSS("transform", "none");
  await expect(button).toHaveCSS("transition-duration", "0s");
});
