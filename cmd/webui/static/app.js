"use strict";

const element = id => document.getElementById(id);
const state = { path: ".", snapshot: "", busy: false };

function join(parent, child) {
  return parent === "." ? child : `${parent}/${child}`;
}

function endpoint(route, params = {}, snapshot = state.snapshot) {
  const query = new URLSearchParams(params);
  if (snapshot) query.set("snapshot", snapshot);
  return `${route}?${query}`;
}

function sizeText(bytes) {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 ** 2) return `${(bytes / 1024).toFixed(1)} KiB`;
  if (bytes < 1024 ** 3) return `${(bytes / 1024 ** 2).toFixed(1)} MiB`;
  return `${(bytes / 1024 ** 3).toFixed(2)} GiB`;
}

function message(text, error = false) {
  element("status").textContent = text;
  element("status").classList.toggle("error", error);
}

function controls() {
  document.querySelectorAll("button, select").forEach(button => { button.disabled = state.busy; });
  document.querySelectorAll("[data-edit]").forEach(button => { button.disabled = state.busy || Boolean(state.snapshot); });
  element("snapshot-delete").disabled = state.busy || !state.snapshot;
  element("snapshot-restore").disabled = state.busy || !state.snapshot;
  element("readonly").hidden = !state.snapshot;
}

async function request(url, options) {
  const headers = new Headers(options?.headers);
  headers.set("X-PackFS", "1");
  const response = await fetch(url, { ...options, headers });
  if (!response.ok) throw new Error((await response.text()).trim() || `HTTP ${response.status}`);
  return response;
}

function post(route, body) {
  return request(endpoint(route, {}, ""), {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
}

async function perform(task) {
  state.busy = true;
  controls();
  message("正在处理…");
  try {
    await task();
    await refresh();
    message("操作完成，修改已提交。");
  } catch (error) {
    message(error.message, true);
  } finally {
    state.busy = false;
    controls();
  }
}

async function navigate(path) {
  const previous = state.path;
  state.path = path;
  state.busy = true;
  controls();
  try {
    await refresh();
    message(state.snapshot ? `正在只读浏览快照：${state.snapshot}` : "正在浏览当前工作树。");
  } catch (error) {
    state.path = previous;
    message(error.message, true);
  } finally {
    state.busy = false;
    controls();
  }
}

function button(text, action, className = "") {
  const result = document.createElement("button");
  result.type = "button";
  result.textContent = text;
  result.className = className;
  result.addEventListener("click", action);
  return result;
}

function breadcrumbs() {
  const nav = element("breadcrumbs");
  nav.replaceChildren(button("根目录", () => navigate(".")));
  if (state.path === ".") return;
  let target = "";
  for (const part of state.path.split("/")) {
    target = target ? `${target}/${part}` : part;
    const destination = target;
    const slash = document.createElement("span");
    slash.textContent = "/";
    nav.append(slash, button(part, () => navigate(destination)));
  }
}

async function refresh() {
  const response = await request(endpoint("/api/list", { path: state.path }));
  const data = await response.json();
  element("container").textContent = data.container;
  element("size").textContent = `容器大小 ${sizeText(data.size)}`;
  const select = element("snapshot");
  select.replaceChildren(new Option("当前工作树", ""));
  for (const name of data.snapshots) select.add(new Option(name, name));
  select.value = state.snapshot;
  breadcrumbs();
  element("download-directory").href = endpoint("/api/download", { path: state.path });
  const table = element("entries");
  table.replaceChildren();
  for (const entry of data.entries) {
    const fullPath = join(state.path, entry.name);
    const row = document.createElement("tr");
    const name = document.createElement("td");
    if (entry.directory) {
      name.append(button(`▸ ${entry.name}`, () => navigate(fullPath), "entry-name"));
    } else {
      const link = document.createElement("a");
      link.textContent = entry.name;
      link.href = endpoint("/api/download", { path: fullPath });
      name.append(link);
    }
    const kind = document.createElement("td");
    kind.textContent = entry.directory ? "目录" : "文件";
    const size = document.createElement("td");
    size.textContent = sizeText(entry.size);
    size.title = `${entry.size} 字节`;
    const actions = document.createElement("td");
    const group = document.createElement("div");
    group.className = "row-actions";
    const download = document.createElement("a");
    download.textContent = entry.directory ? "ZIP 下载" : "下载";
    download.href = endpoint("/api/download", { path: fullPath });
    group.append(download);
    if (!state.snapshot) {
      group.append(button("重命名", () => {
        const target = prompt("新的完整容器路径：", fullPath);
        if (target !== null) perform(() => post("/api/rename", { old: fullPath, new: target }));
      }));
      group.append(button("删除", () => {
        if (entry.directory && !confirm(`递归删除目录“${fullPath}”及其全部内容？`)) return;
        perform(() => post("/api/remove", { path: fullPath, recursive: entry.directory }));
      }, "danger"));
    }
    actions.append(group);
    row.append(name, kind, size, actions);
    table.append(row);
  }
  element("empty").hidden = data.entries.length !== 0;
  controls();
}

async function upload(files) {
  await perform(async () => {
    let index = 0;
    for (const file of files) {
      const relative = file.webkitRelativePath || file.name;
      message(`正在上传 ${++index}/${files.length}：${relative}`);
      await request(endpoint("/api/upload", { path: join(state.path, relative) }), { method: "POST", body: file });
    }
  });
}

element("refresh").addEventListener("click", () => navigate(state.path));
element("snapshot").addEventListener("change", () => {
  state.snapshot = element("snapshot").value;
  navigate(".");
});
element("mkdir").addEventListener("click", () => {
  const name = prompt("新目录名称或相对路径：");
  if (name !== null) perform(() => post("/api/mkdir", { path: join(state.path, name) }));
});
element("upload-files").addEventListener("click", () => element("files").click());
element("upload-folder").addEventListener("click", () => element("folder").click());
for (const id of ["files", "folder"]) {
  element(id).addEventListener("change", async event => {
    await upload(Array.from(event.target.files));
    event.target.value = "";
  });
}
element("snapshot-create").addEventListener("click", () => {
  const name = prompt("快照名字：");
  if (name !== null) perform(() => post("/api/snapshot/create", { name }));
});
element("snapshot-delete").addEventListener("click", () => {
  const name = state.snapshot;
  if (!confirm(`删除快照“${name}”？`)) return;
  perform(async () => {
    await post("/api/snapshot/delete", { name });
    state.snapshot = "";
    state.path = ".";
  });
});
element("snapshot-restore").addEventListener("click", () => {
  const name = state.snapshot;
  if (!confirm(`把当前工作树恢复为快照“${name}”？当前工作树将被替换。`)) return;
  perform(async () => {
    await post("/api/snapshot/restore", { name });
    state.snapshot = "";
    state.path = ".";
  });
});
element("compact").addEventListener("click", () => perform(() => post("/api/compact", {})));
navigate(".");
