const state = {
  tasks: [],
  filter: "all",
  query: "",
  selectedCategory: null,
  selectedSignature: "",
  categorySignature: "",
  loading: false
};

const labels = {
  queued: "等待中",
  classifying: "AI 分类中",
  downloading: "下载中",
  succeeded: "已保存",
  failed: "失败"
};

const $ = (selector) => document.querySelector(selector);

function formatBytes(bytes, fallback = "0 B") {
  const input = Number(bytes);
  if (!Number.isFinite(input) || input < 0) return fallback;
  const units = ["B", "KB", "MB", "GB", "TB"];
  let value = input;
  let unit = 0;
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024;
    unit += 1;
  }
  const digits = value >= 10 || unit === 0 ? 0 : 1;
  return `${value.toFixed(digits)} ${units[unit]}`;
}

function formatTime(value, withDate = false) {
  if (!value) return "";
  const options = withDate
    ? { month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit" }
    : { hour: "2-digit", minute: "2-digit" };
  return new Intl.DateTimeFormat("zh-CN", options).format(new Date(value));
}

function formatDay(value) {
  if (!value) return "";
  return new Intl.DateTimeFormat("zh-CN", {
    year: "numeric", month: "long", day: "numeric"
  }).format(new Date(value));
}

function dayKey(value) {
  const date = new Date(value || 0);
  return `${date.getFullYear()}-${date.getMonth()}-${date.getDate()}`;
}

function activeStatus(status) {
  return ["queued", "classifying", "downloading"].includes(status);
}

function progressPercent(task) {
  const total = Number(task.total_bytes) || 0;
  if (total <= 0) return null;
  return Math.max(0, Math.min(100, (Number(task.downloaded_bytes || 0) / total) * 100));
}

function currentSpeed(task) {
  if (task.status !== "downloading" || !task.updated_at) return 0;
  return Date.now() - new Date(task.updated_at).getTime() <= 3500
    ? Number(task.current_speed_bps) || 0
    : 0;
}

function categoryName(task) {
  return task.directory?.trim() || "待分类";
}

function categoryInitial(category) {
  const leaf = category.split("/").filter(Boolean).pop() || "TG";
  return [...leaf].slice(0, 2).join("").toUpperCase();
}

function taskTitle(task) {
  return task.title || task.caption?.split("\n").find(Boolean) || task.archive_name || "未命名归档";
}

function taskSubtitle(task) {
  if (task.status === "downloading") {
    const percent = progressPercent(task);
    return percent === null ? "正在下载…" : `正在下载 ${percent.toFixed(1)}%`;
  }
  if (task.error) return task.error;
  if (task.caption) return task.caption.replace(/\s+/g, " ");
  const count = task.preview?.length || task.media_count || 0;
  return count ? `${count} 个媒体文件` : "暂无描述";
}

function matches(task) {
  if (state.filter === "active" && !activeStatus(task.status)) return false;
  if (!["all", "active"].includes(state.filter) && task.status !== state.filter) return false;
  if (!state.query) return true;
  const values = [
    task.title,
    task.caption,
    task.directory,
    task.archive_name,
    task.error,
    ...(task.saved_files || []),
    ...(task.failed_files || [])
  ];
  return values.join(" ").toLowerCase().includes(state.query);
}

function visibleTasks() {
  return state.tasks.filter(matches);
}

function categoryGroups(tasks = visibleTasks()) {
  const groups = new Map();
  tasks.forEach((task) => {
    const name = categoryName(task);
    if (!groups.has(name)) groups.set(name, []);
    groups.get(name).push(task);
  });
  return [...groups.entries()]
    .map(([name, items]) => {
      const ordered = [...items].sort((a, b) => new Date(a.created_at) - new Date(b.created_at));
      const latest = [...items].sort((a, b) => new Date(b.updated_at) - new Date(a.updated_at))[0];
      return { name, items: ordered, latest };
    })
    .sort((a, b) => new Date(b.latest.updated_at) - new Date(a.latest.updated_at));
}

function element(tag, className, text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== undefined) node.textContent = text;
  return node;
}

function categoryListSignature(groups) {
  const contents = groups
    .map((group) => [
      group.name,
      group.items
        .map((task) => [
          task.id,
          task.status,
          task.title,
          task.caption,
          task.error,
          task.retryable,
          task.resumable,
          task.preview?.length || 0
        ])
        .sort((a, b) => String(a[0]).localeCompare(String(b[0])))
    ])
    .sort((a, b) => a[0].localeCompare(b[0]));
  return JSON.stringify([state.filter, state.query, state.selectedCategory, contents]);
}

function updateCategoryListLive(groups) {
  const buttons = [...document.querySelectorAll("#taskList [data-category]")];
  groups.forEach((group) => {
    const button = buttons.find((item) => item.dataset.category === group.name);
    if (!button) return;
    const time = button.querySelector('[data-role="category-time"]');
    const snippet = button.querySelector('[data-role="category-snippet"]');
    const count = button.querySelector('[data-role="category-count"]');
    if (time) time.textContent = formatTime(group.latest.updated_at);
    if (snippet) snippet.textContent = `${taskTitle(group.latest)} · ${taskSubtitle(group.latest)}`;
    if (count) count.textContent = String(group.items.length);
  });
}

function renderCategoryList(force = false) {
  const list = $("#taskList");
  const groups = categoryGroups();
  const signature = categoryListSignature(groups);
  if (!force && signature === state.categorySignature) {
    updateCategoryListLive(groups);
    return;
  }
  state.categorySignature = signature;
  list.replaceChildren();
  if (!groups.length) {
    list.append($("#emptyTemplate").content.cloneNode(true));
    return;
  }

  groups.forEach((group) => {
    const button = element("button", `chat-item${group.name === state.selectedCategory ? " selected" : ""}`);
    button.type = "button";
    button.dataset.category = group.name;

    const thumb = element("div", "chat-thumb category-thumb", categoryInitial(group.name));
    const body = element("div", "chat-item-body");
    const top = element("div", "chat-item-top");
    const updatedAt = element("time", "", formatTime(group.latest.updated_at));
    updatedAt.dataset.role = "category-time";
    top.append(element("strong", "", group.name), updatedAt);
    const bottom = element("div", "chat-item-bottom");
    const snippet = element("span", "chat-snippet", `${taskTitle(group.latest)} · ${taskSubtitle(group.latest)}`);
    snippet.dataset.role = "category-snippet";
    const count = element("span", "media-count", String(group.items.length));
    count.dataset.role = "category-count";
    bottom.append(snippet, count);
    body.append(top, bottom);
    button.append(thumb, body);
    button.addEventListener("click", () => selectCategory(group.name));
    list.append(button);
  });
}

function albumClass(count) {
  return `media-album count-${Math.min(count, 5)}`;
}

function renderMedia(task) {
  const media = task.preview || [];
  if (!media.length) return null;
  const album = element("div", albumClass(media.length));
  media.forEach((item, index) => {
    const frame = element("div", `media-frame ${item.kind}`);
    frame.dataset.order = String(index + 1);
    if (item.kind === "image") {
      const image = document.createElement("img");
      image.src = item.url;
      image.alt = `${taskTitle(task)}，第 ${index + 1} 张图片`;
      image.loading = index > 1 ? "lazy" : "eager";
      frame.append(image);
    } else {
      const video = document.createElement("video");
      video.src = item.url;
      video.controls = true;
      video.preload = "metadata";
      video.playsInline = true;
      video.setAttribute("aria-label", `${taskTitle(task)}，第 ${index + 1} 个视频`);
      frame.append(video);
    }
    const order = element("span", "media-order", String(index + 1));
    order.title = `Telegram 消息顺序：${index + 1}`;
    frame.append(order);
    album.append(frame);
  });
  return album;
}

function renderProgress(task, container) {
  if (!activeStatus(task.status) && task.status !== "failed") return;
  const box = element("div", "download-card");
  const percent = progressPercent(task);
  const row = element("div", "download-row");
  const currentFile = element("span", "", task.current_file || labels[task.status]);
  currentFile.dataset.role = "current-file";
  const progressText = element("strong", "", percent === null ? "等待数据" : `${percent.toFixed(1)}%`);
  progressText.dataset.role = "progress-text";
  row.append(currentFile, progressText);
  box.append(row);
  const track = element("div", `progress-track${percent === null ? " indeterminate" : ""}`);
  track.dataset.role = "progress-track";
  if (percent !== null) {
    const bar = element("div", "progress-bar");
    bar.style.width = `${percent}%`;
    track.append(bar);
  }
  box.append(track);
  const stats = element("div", "download-stats");
  const transferred = element("span", "", `${formatBytes(task.downloaded_bytes || 0)} / ${formatBytes(task.total_bytes, "大小未知")}`);
  transferred.dataset.role = "transferred";
  const speed = element("span", "", `${formatBytes(currentSpeed(task))}/s`);
  speed.dataset.role = "speed";
  stats.append(transferred, speed);
  box.append(stats);
  if (task.error) box.append(element("p", "task-error", task.error));
  if (task.status === "failed" && task.retryable) {
    const actions = element("div", "retry-actions");
    if (task.resumable) {
      const resume = element("button", "retry-button resume-button", "继续下载");
      resume.type = "button";
      resume.addEventListener("click", () => retryTask(task.id, resume, "resume"));
      actions.append(resume);
    }
    const retry = element("button", "retry-button", "重新下载");
    retry.type = "button";
    retry.addEventListener("click", () => retryTask(task.id, retry, "retry"));
    actions.append(retry);
    box.append(actions);
  }
  container.append(box);
}

function renderMessage(task) {
  const row = element("div", "message-row");
  const bubble = element("article", `message-bubble status-${task.status}${(task.preview || []).length ? " has-media" : ""}`);
  bubble.dataset.taskId = task.id;

  const media = renderMedia(task);
  if (media) bubble.append(media);

  const content = element("div", "message-content");
  content.append(element("h2", "archive-title", taskTitle(task)));
  if (task.caption) content.append(element("p", "message-caption", task.caption));
  if (!task.caption && !media && !activeStatus(task.status)) {
    content.append(element("p", "message-caption muted", "这条归档没有可预览的媒体或描述。"));
  }
  renderProgress(task, content);
  bubble.append(content);

  const footer = element("footer", "message-footer");
  const detail = `${task.media_count || 0} 个媒体 · ${formatBytes(task.total_bytes, "大小未知")}`;
  const detailNode = element("span", "message-detail", detail);
  detailNode.dataset.role = "message-detail";
  footer.append(detailNode);
  if (task.status !== "succeeded") {
    footer.append(element("span", `message-status ${task.status}`, labels[task.status] || task.status));
  }
  const updatedAt = element("time", "", formatTime(task.updated_at));
  updatedAt.dataset.role = "task-updated-at";
  footer.append(updatedAt);
  if (task.status === "succeeded") footer.append(element("span", "check", "✓✓"));
  bubble.append(footer);
  row.append(bubble);
  return row;
}

function tasksSignature(tasks) {
  return JSON.stringify(tasks.map((task) => [
    task.id,
    task.status,
    task.created_at,
    task.title,
    task.caption,
    task.error,
    task.directory,
    task.archive_name,
    task.media_count,
    task.total_bytes,
    task.retryable,
    task.resumable,
    task.saved_files,
    task.failed_files,
    ...(task.preview || []).map((item) => [item.url, item.kind, item.size])
  ]));
}

function updateLiveTaskNodes(tasks, stage = $("#messageStage")) {
  const bubbles = [...stage.querySelectorAll("[data-task-id]")];
  tasks.forEach((task) => {
    const bubble = bubbles.find((item) => item.dataset.taskId === task.id);
    if (!bubble) return;

    const percent = progressPercent(task);
    const currentFile = bubble.querySelector('[data-role="current-file"]');
    const progressText = bubble.querySelector('[data-role="progress-text"]');
    const track = bubble.querySelector('[data-role="progress-track"]');
    const transferred = bubble.querySelector('[data-role="transferred"]');
    const speed = bubble.querySelector('[data-role="speed"]');
    const detail = bubble.querySelector('[data-role="message-detail"]');
    const updatedAt = bubble.querySelector('[data-role="task-updated-at"]');

    if (currentFile) currentFile.textContent = task.current_file || labels[task.status];
    if (progressText) progressText.textContent = percent === null ? "等待数据" : `${percent.toFixed(1)}%`;
    if (track) {
      track.classList.toggle("indeterminate", percent === null);
      let bar = track.querySelector(".progress-bar");
      if (percent === null) {
        bar?.remove();
      } else {
        if (!bar) {
          bar = element("div", "progress-bar");
          track.append(bar);
        }
        bar.style.width = `${percent}%`;
      }
    }
    if (transferred) {
      transferred.textContent = `${formatBytes(task.downloaded_bytes || 0)} / ${formatBytes(task.total_bytes, "大小未知")}`;
    }
    if (speed) speed.textContent = `${formatBytes(currentSpeed(task))}/s`;
    if (detail) detail.textContent = `${task.media_count || 0} 个媒体 · ${formatBytes(task.total_bytes, "大小未知")}`;
    if (updatedAt) updatedAt.textContent = formatTime(task.updated_at);
  });
}

function showEmptyConversation() {
  const header = $("#conversationHeader");
  header.querySelector(".conversation-title strong").textContent = "选择一个分类";
  header.querySelector(".conversation-title span").textContent = "分类中的归档目录会显示为一条条消息";
  const status = $("#headerStatus");
  status.className = "status-pill";
  status.textContent = "等待选择";
  const stage = $("#messageStage");
  stage.replaceChildren();
  const welcome = element("div", "welcome");
  welcome.append(
    element("div", "welcome-icon", "✦"),
    element("h2", "", "按分类浏览归档"),
    element("p", "", "从左侧选择一个分类；该分类下的每个归档目录都是一条 Telegram 风格消息。")
  );
  stage.append(welcome);
}

function renderSelectedCategory(force = false, scrollToBottom = false) {
  const group = categoryGroups().find((item) => item.name === state.selectedCategory);
  if (!group) {
    state.selectedSignature = "";
    showEmptyConversation();
    return;
  }
  const signature = tasksSignature(group.items);
  const stage = $("#messageStage");
  if (!force && signature === state.selectedSignature) {
    updateLiveTaskNodes(group.items, stage);
    return;
  }
  state.selectedSignature = signature;
  const previousScrollTop = stage.scrollTop;
  const wasNearBottom = stage.scrollHeight - stage.scrollTop - stage.clientHeight < 80;

  const activeCount = group.items.filter((task) => activeStatus(task.status)).length;
  const failedCount = group.items.filter((task) => task.status === "failed").length;
  const mediaCount = group.items.reduce((sum, task) => sum + (task.preview?.length || 0), 0);
  const header = $("#conversationHeader");
  header.querySelector(".conversation-title strong").textContent = group.name;
  header.querySelector(".conversation-title span").textContent = `${group.items.length} 条归档 · ${mediaCount} 个可预览媒体`;
  const status = $("#headerStatus");
  if (activeCount) {
    status.className = "status-pill downloading";
    status.textContent = `${activeCount} 条进行中`;
  } else if (failedCount) {
    status.className = "status-pill failed";
    status.textContent = `${failedCount} 条失败`;
  } else {
    status.className = "status-pill succeeded";
    status.textContent = `${group.items.length} 条消息`;
  }

  stage.replaceChildren();
  let previousDay = "";
  group.items.forEach((task) => {
    const currentDay = dayKey(task.created_at);
    if (currentDay !== previousDay) {
      stage.append(element("div", "day-divider", formatDay(task.created_at)));
      previousDay = currentDay;
    }
    stage.append(renderMessage(task));
  });
  if (scrollToBottom || wasNearBottom) {
    stage.scrollTop = stage.scrollHeight;
  } else {
    stage.scrollTop = previousScrollTop;
  }
}

function selectCategory(category) {
  state.selectedCategory = category;
  state.selectedSignature = "";
  renderCategoryList();
  renderSelectedCategory(true, true);
  if (window.innerWidth <= 760) {
    $(".conversation").scrollIntoView({ behavior: "smooth", block: "start" });
  }
}

function chooseDefaultCategory() {
  const groups = categoryGroups();
  if (groups.some((group) => group.name === state.selectedCategory)) return false;
  const preferred = groups.find((group) => group.items.some((task) => (task.preview || []).length > 0));
  state.selectedCategory = preferred?.name || groups[0]?.name || null;
  state.selectedSignature = "";
  return true;
}

function updateMetrics(summary = {}, transfer = {}) {
  $("#countAll").textContent = state.tasks.length;
  $("#countActive").textContent = (summary.queued || 0) + (summary.classifying || 0) + (summary.downloading || 0);
  $("#countSucceeded").textContent = summary.succeeded || 0;
  $("#countFailed").textContent = summary.failed || 0;
  $("#bandwidth").textContent = `${formatBytes(transfer.active_speed_bps || 0)}/s`;
}

async function refresh() {
  if (state.loading) return;
  state.loading = true;
  try {
    const response = await fetch("/api/tasks", { cache: "no-store" });
    if (!response.ok) throw new Error(`HTTP ${response.status}`);
    const data = await response.json();
    state.tasks = data.tasks || [];
    const selectionChanged = chooseDefaultCategory();
    updateMetrics(data.summary, data.transfer);
    renderCategoryList();
    renderSelectedCategory(false, selectionChanged);
    $("#lastUpdated").textContent = `同步于 ${formatTime(data.generated_at)}`;
    $(".connection").classList.add("online");
    $("#connectionText").textContent = "已连接";
  } catch (error) {
    $(".connection").classList.remove("online");
    $("#connectionText").textContent = "连接中断";
    $("#lastUpdated").textContent = error.message;
  } finally {
    state.loading = false;
  }
}

async function retryTask(id, button, action) {
  button.disabled = true;
  const resuming = action === "resume";
  button.textContent = resuming ? "正在继续入队…" : "正在重新入队…";
  try {
    const response = await fetch(`/api/tasks/${encodeURIComponent(id)}/${action}`, { method: "POST" });
    const data = await response.json();
    if (!response.ok) throw new Error(data.error || `HTTP ${response.status}`);
    state.selectedSignature = "";
    await refresh();
  } catch (error) {
    button.disabled = false;
    button.textContent = resuming ? "继续失败，再试一次" : "重试失败，再试一次";
    button.title = error.message;
  }
}

document.querySelectorAll(".filter").forEach((button) => {
  button.addEventListener("click", () => {
    document.querySelectorAll(".filter").forEach((item) => item.classList.remove("active"));
    button.classList.add("active");
    state.filter = button.dataset.filter;
    const selectionChanged = chooseDefaultCategory();
    renderCategoryList();
    renderSelectedCategory(true, selectionChanged);
  });
});

$("#searchInput").addEventListener("input", (event) => {
  state.query = event.target.value.trim().toLowerCase();
  const selectionChanged = chooseDefaultCategory();
  renderCategoryList();
  renderSelectedCategory(true, selectionChanged);
});

refresh();
setInterval(() => {
  if (!document.hidden) refresh();
}, 2000);
document.addEventListener("visibilitychange", () => {
  if (!document.hidden) refresh();
});
