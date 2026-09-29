import { expect, type Page } from "@playwright/test";

export async function expectTextContrast(page: Page) {
  const failures = await page
    .locator(
      ".phase, .log-line code, .log-line-number, .ui-button, .ui-nav, a, p, h1, h2, select, textarea, input, label",
    )
    .evaluateAll((nodes) => {
      const rgb = (s: string) => (s.match(/[\d.]+/g) ?? []).map(Number);
      const luminance = (s: string) =>
        rgb(s)
          .slice(0, 3)
          .map((x) => x / 255)
          .map((x) => (x <= 0.04045 ? x / 12.92 : ((x + 0.055) / 1.055) ** 2.4))
          .reduce((sum, x, i) => sum + x * [0.2126, 0.7152, 0.0722][i], 0);
      return nodes.flatMap((node) => {
        if (!node.getClientRects().length) return [];
        let parent: Element | null = node;
        while (parent && rgb(getComputedStyle(parent).backgroundColor)[3] === 0)
          parent = parent.parentElement;
        const b = luminance(
          parent
            ? getComputedStyle(parent).backgroundColor
            : "rgb(255,255,255)",
        );
        const colors = [getComputedStyle(node).color];
        if (node.hasAttribute("placeholder"))
          colors.push(getComputedStyle(node, "::placeholder").color);
        return colors.flatMap((color) => {
          const a = luminance(color);
          const ratio = (Math.max(a, b) + 0.05) / (Math.min(a, b) + 0.05);
          return ratio < 4.5
            ? [`${node.textContent?.trim().slice(0, 35)}: ${ratio.toFixed(2)}`]
            : [];
        });
      });
    });
  expect(failures).toEqual([]);
}
