// prev/next pager shared by the connections and statements tables. Takes the
// offset setter as a parameter so neither this nor its callers import the
// store.

export function renderPager(container, total, offset, pageSize, onPage) {
  if (!container) return;
  container.innerHTML = "";
  if (total <= pageSize) return; // nothing to page through

  const prev = document.createElement("button");
  prev.className = "btn";
  prev.textContent = "‹ Previous";
  prev.disabled = offset === 0;
  prev.addEventListener("click", () => onPage(offset - pageSize));

  const next = document.createElement("button");
  next.className = "btn";
  next.textContent = "Next ›";
  next.disabled = offset + pageSize >= total;
  next.addEventListener("click", () => onPage(offset + pageSize));

  const label = document.createElement("span");
  label.className = "count";
  label.textContent = `page ${Math.floor(offset / pageSize) + 1} of ${Math.ceil(total / pageSize)}`;

  container.appendChild(prev);
  container.appendChild(label);
  container.appendChild(next);
}