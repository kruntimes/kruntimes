(function () {
  function copyText(text) {
    if (navigator.clipboard && window.isSecureContext) {
      return navigator.clipboard.writeText(text);
    }

    var textarea = document.createElement("textarea");
    textarea.value = text;
    textarea.setAttribute("readonly", "");
    textarea.style.position = "absolute";
    textarea.style.left = "-9999px";
    document.body.appendChild(textarea);
    textarea.select();

    try {
      document.execCommand("copy");
      return Promise.resolve();
    } catch (error) {
      return Promise.reject(error);
    } finally {
      document.body.removeChild(textarea);
    }
  }

  function labelFor(button, copied) {
    button.textContent = copied ? button.dataset.copiedLabel : button.dataset.copyLabel;
    button.setAttribute("aria-label", button.textContent);
  }

  document.addEventListener("DOMContentLoaded", function () {
    document.querySelectorAll(".content pre").forEach(function (pre) {
      var code = pre.querySelector("code");
      if (!code || pre.querySelector(".copy-code-button")) {
        return;
      }

      var button = document.createElement("button");
      button.type = "button";
      button.className = "copy-code-button";
      button.dataset.copyLabel = document.documentElement.lang === "zh-cn" ? "复制" : "Copy";
      button.dataset.copiedLabel = document.documentElement.lang === "zh-cn" ? "已复制" : "Copied";
      labelFor(button, false);

      button.addEventListener("click", function () {
        copyText(code.innerText).then(function () {
          labelFor(button, true);
          window.setTimeout(function () {
            labelFor(button, false);
          }, 1600);
        });
      });

      pre.classList.add("has-copy-button");
      pre.appendChild(button);
    });
  });
})();
