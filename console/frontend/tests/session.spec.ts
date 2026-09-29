import { expect, test } from "@playwright/test";
import { readFile } from "node:fs/promises";

test("login moves the token form to a dialog and displays the authenticated account", async ({
  page,
}) => {
  await page.route("**/*", async (route) => {
    if (route.request().resourceType() === "document") {
      return route.fulfill({
        contentType: "text/html",
        body: await readFile("../backend/assets/index.html"),
      });
    }
    const path = new URL(route.request().url()).pathname;
    if (!path.startsWith("/api/")) return route.continue();
    if (path === "/api/session" && route.request().method() === "POST") {
      return route.fulfill({
        json: {
          authenticated: true,
          accountName: "system:serviceaccount:team-a:viewer",
        },
      });
    }
    return route.fulfill({
      json:
        path === "/api/namespaces"
          ? { items: ["default"] }
          : { authenticated: false },
    });
  });

  await page.goto("/");
  await expect(
    page.getByRole("button", { name: "Log in", exact: true }),
  ).toBeVisible();
  await expect(
    page.getByText("Connect for protected details and logs"),
  ).toHaveCount(0);

  await page.getByRole("button", { name: "Log in", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "Log in" });
  await dialog.getByLabel("Kubernetes bearer token").fill("caller-token");
  await dialog.getByRole("button", { name: "Log in", exact: true }).click();

  await expect(dialog).toHaveCount(0);
  await expect(page.getByText("team-a/viewer", { exact: true })).toBeVisible();
  await expect(
    page.getByRole("button", { name: "Log out", exact: true }),
  ).toBeVisible();
});
