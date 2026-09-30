import { expect, test } from "@playwright/test";
import { readFile } from "node:fs/promises";

const sessionRun = {
  name: "shell",
  namespace: "default",
  uid: "shell-1",
  runtime: "bash",
  mode: "Session",
  phase: "Ready",
  creationTimestamp: "2026-09-01T00:00:00Z",
  spec: { mode: { session: {} } },
  status: { phase: "Ready" },
};

test.beforeEach(async ({ page }) => {
  await page.addInitScript(() => {
    class MockWebSocket extends EventTarget {
      static CONNECTING = 0;
      static OPEN = 1;
      static CLOSING = 2;
      static CLOSED = 3;
      readyState = MockWebSocket.CONNECTING;
      sent: string[] = [];

      constructor(readonly url: string) {
        super();
        setTimeout(() => {
          this.readyState = MockWebSocket.OPEN;
          this.dispatchEvent(new Event("open"));
        });
      }

      send(value: string) {
        this.sent.push(value);
        const message = JSON.parse(value) as {
          type?: string;
          command?: { shell?: string };
        };
        if (message.type === "cancel") {
          this.close();
          return;
        }
        const event = (data: unknown) =>
          this.dispatchEvent(
            new MessageEvent("message", { data: JSON.stringify(data) }),
          );
        event({
          sequence: 1,
          type: "accepted",
          accepted: { operationId: "operation-1" },
        });
        if (message.command?.shell === "sleep 30") return;
        event({
          sequence: 2,
          type: "output",
          output: { stream: "stdout", data: "aGVsbG8K" },
        });
        event({
          sequence: 3,
          type: "completed",
          completed: {
            command: {
              exitCode: 0,
              stdout: "",
              stderr: "",
              timedOut: false,
            },
          },
        });
        this.close();
      }

      close() {
        if (this.readyState === MockWebSocket.CLOSED) return;
        this.readyState = MockWebSocket.CLOSED;
        this.dispatchEvent(new CloseEvent("close"));
      }
    }
    Object.defineProperty(window, "WebSocket", { value: MockWebSocket });
  });
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
      "/api/namespaces/default/runs/shell": sessionRun,
    };
    return route.fulfill({ json: data[path] ?? { items: [] } });
  });
});

test("Session Run command panel streams ordered WebSocket events", async ({
  page,
}) => {
  await page.goto("/namespaces/default/runs/shell");
  await page.getByLabel("Session shell command").fill("printf hello");
  await page.getByRole("button", { name: "Run command" }).click();

  const events = page.getByLabel("Session operation events");
  await expect(events).toContainText("accepted: operation-1");
  await expect(events).toContainText("[stdout] hello");
  await expect(events).toContainText("completed: exit code 0");
  await expect(page.getByText("Operation completed.")).toBeVisible();
});

test("Session Run command panel sends a cancellation control frame", async ({
  page,
}) => {
  await page.goto("/namespaces/default/runs/shell");
  await page.getByLabel("Session shell command").fill("sleep 30");
  await page.getByRole("button", { name: "Run command" }).click();
  await expect(
    page.getByRole("button", { name: "Cancel operation" }),
  ).toBeVisible();
  await page.getByRole("button", { name: "Cancel operation" }).click();

  await expect(page.getByText("Operation cancelled.")).toBeVisible();
});
