import "./styles/stripe.css";
import "./styles/neumorphism.css";
import "./styles/github.css";

export const styles = [
  { id: "github", label: "GitHub" },
  { id: "stripe", label: "Stripe-inspired" },
  { id: "neumorphism", label: "Neumorphism" },
] as const;
export type UIStyle = (typeof styles)[number]["id"];
export type Theme = "light" | "dark" | "system";
export const defaultStyle: UIStyle = "github";

function read(key: string): string | null {
  try {
    return localStorage.getItem(key);
  } catch {
    return null;
  }
}
function save(key: string, value: string) {
  try {
    localStorage.setItem(key, value);
  } catch {
    /* Preferences still work in memory. */
  }
}
const savedStyle = read("kruntimes-console-style");
const savedTheme = read("kruntimes-console-theme");
export const initialStyle: UIStyle =
  styles.find((style) => style.id === savedStyle)?.id ?? defaultStyle;
export const initialTheme: Theme =
  savedTheme === "light" || savedTheme === "dark" ? savedTheme : "system";
const systemTheme = matchMedia("(prefers-color-scheme: dark)");
let currentTheme = initialTheme;

function applyTheme(theme: Theme) {
  document.documentElement.dataset.theme = theme;
  document.documentElement.dataset.colorScheme =
    theme === "system" ? (systemTheme.matches ? "dark" : "light") : theme;
}
// Apply before React renders, including on direct detail-page navigation.
document.documentElement.dataset.style = initialStyle;
applyTheme(initialTheme);
systemTheme.addEventListener("change", () => applyTheme(currentTheme));

export function selectStyle(style: UIStyle) {
  document.documentElement.dataset.style = style;
  save("kruntimes-console-style", style);
}
export function selectTheme(theme: Theme) {
  currentTheme = theme;
  applyTheme(theme);
  save("kruntimes-console-theme", theme);
}
