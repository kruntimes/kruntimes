(function () {
  function normalize(value) {
    return value.toLowerCase().replace(/\s+/g, " ").trim();
  }

  function scorePage(page, terms) {
    var title = normalize(page.title || "");
    var summary = normalize(page.summary || "");
    var content = normalize(page.content || "");
    var score = 0;

    for (var i = 0; i < terms.length; i += 1) {
      var term = terms[i];
      if (!term) {
        continue;
      }
      if (title.indexOf(term) !== -1) {
        score += 8;
      }
      if (summary.indexOf(term) !== -1) {
        score += 4;
      }
      if (content.indexOf(term) !== -1) {
        score += 1;
      }
    }

    return score;
  }

  function resultSummary(page) {
    var text = page.summary || page.content || "";
    return text.replace(/\s+/g, " ").trim().slice(0, 140);
  }

  function renderResults(container, results, emptyText) {
    container.textContent = "";

    if (!results.length) {
      var empty = document.createElement("div");
      empty.className = "docs-search-empty";
      empty.textContent = emptyText;
      container.appendChild(empty);
      return;
    }

    results.slice(0, 8).forEach(function (page) {
      var link = document.createElement("a");
      link.className = "docs-search-result";
      link.href = page.url;
      link.setAttribute("role", "listitem");

      var title = document.createElement("strong");
      title.textContent = page.title;
      link.appendChild(title);

      var summary = resultSummary(page);
      if (summary) {
        var span = document.createElement("span");
        span.textContent = summary;
        link.appendChild(span);
      }

      container.appendChild(link);
    });
  }

  function attachSearch(root) {
    var input = root.querySelector("input[type='search']");
    var results = root.querySelector(".docs-search-results");
    var indexUrl = root.getAttribute("data-index-url");
    var emptyText = root.getAttribute("data-empty-text") || "No matching results";
    var pagesPromise;

    function loadPages() {
      if (!pagesPromise) {
        pagesPromise = fetch(indexUrl).then(function (response) {
          if (!response.ok) {
            throw new Error("failed to load search index");
          }
          return response.json();
        });
      }
      return pagesPromise;
    }

    function update() {
      var query = normalize(input.value || "");
      if (!query) {
        results.textContent = "";
        root.removeAttribute("data-has-results");
        return;
      }

      root.setAttribute("data-has-results", "true");
      loadPages()
        .then(function (pages) {
          var terms = query.split(" ");
          var matches = pages
            .map(function (page) {
              return { page: page, score: scorePage(page, terms) };
            })
            .filter(function (item) {
              return item.score > 0;
            })
            .sort(function (left, right) {
              return right.score - left.score || left.page.title.localeCompare(right.page.title);
            })
            .map(function (item) {
              return item.page;
            });
          renderResults(results, matches, emptyText);
        })
        .catch(function () {
          renderResults(results, [], emptyText);
        });
    }

    input.addEventListener("input", update);
    input.addEventListener("focus", function () {
      if (input.value) {
        update();
      }
    });
  }

  document.addEventListener("DOMContentLoaded", function () {
    document.querySelectorAll("[data-docs-search]").forEach(attachSearch);
  });
})();
