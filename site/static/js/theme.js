(function () {
  var storageKey = "kruntimes-docs-theme";
  var mediaQuery = window.matchMedia ? window.matchMedia("(prefers-color-scheme: dark)") : null;

  function storedChoice() {
    var choice = "system";
    try {
      choice = localStorage.getItem(storageKey) || "system";
    } catch (error) {
      return "system";
    }
    if (choice !== "light" && choice !== "dark" && choice !== "system") {
      return "system";
    }
    return choice;
  }

  function saveChoice(choice) {
    try {
      if (choice === "system") {
        localStorage.removeItem(storageKey);
      } else {
        localStorage.setItem(storageKey, choice);
      }
    } catch (error) {
      return;
    }
  }

  function effectiveTheme(choice) {
    if (choice === "light" || choice === "dark") {
      return choice;
    }
    return mediaQuery && mediaQuery.matches ? "dark" : "light";
  }

  function applyTheme(choice) {
    var theme = effectiveTheme(choice);
    document.documentElement.dataset.theme = theme;
    document.documentElement.style.colorScheme = theme;
    document.querySelectorAll("[data-theme-select]").forEach(function (select) {
      select.value = choice;
    });
  }

  document.addEventListener("DOMContentLoaded", function () {
    var choice = storedChoice();
    applyTheme(choice);

    document.querySelectorAll("[data-theme-select]").forEach(function (select) {
      select.addEventListener("change", function () {
        choice = select.value;
        saveChoice(choice);
        applyTheme(choice);
      });
    });

    if (mediaQuery && mediaQuery.addEventListener) {
      mediaQuery.addEventListener("change", function () {
        if (storedChoice() === "system") {
          applyTheme("system");
        }
      });
    }
  });
})();
