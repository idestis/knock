import "./style.css";

const status = document.getElementById("copy-status");

async function copyText(text: string): Promise<boolean> {
  try {
    await navigator.clipboard.writeText(text);
    return true;
  } catch {
    // Older browsers and non-secure origins: fall back to a hidden textarea.
    const area = document.createElement("textarea");
    area.value = text;
    area.setAttribute("readonly", "");
    area.style.position = "fixed";
    area.style.opacity = "0";
    document.body.append(area);
    area.select();
    let ok = false;
    try {
      ok = document.execCommand("copy");
    } catch {
      ok = false;
    }
    area.remove();
    return ok;
  }
}

for (const button of document.querySelectorAll<HTMLButtonElement>("button[data-copy]")) {
  const label = button.getAttribute("aria-label") ?? "Copy";
  let timer: number | undefined;

  button.addEventListener("click", async () => {
    const text = button.dataset.copy ?? "";
    const ok = await copyText(text);
    if (status) status.textContent = ok ? "Copied." : "Couldn't copy. Select the text instead.";
    if (!ok) return;

    button.setAttribute("data-copied", "");
    button.setAttribute("aria-label", "Copied");
    window.clearTimeout(timer);
    timer = window.setTimeout(() => {
      button.removeAttribute("data-copied");
      button.setAttribute("aria-label", label);
    }, 1600);
  });
}
