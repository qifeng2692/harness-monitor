// harness-monitor GUI 前端逻辑（原生 JS，无框架依赖）。
// 三级导航：harness → 会话 → 时间线；支持内容全文展开、搜索、live 跟随、回放。

"use strict";

// ---- 全局状态 ----
const state = {
  harnesses: [],
  currentHarness: "",
  sessions: [],
  currentSession: null,
  events: [],
  kindFilter: "",
  live: false,
  liveTimer: null,
  replay: false,
  replayIndex: 0,
  expanded: new Set(), // 已展开步骤的 seq
  searchQuery: "",
};

// ---- 工具 ----
const $ = (id) => document.getElementById(id);

function setStatus(text) { $("statusText").textContent = text; }

function esc(s) {
  return String(s ?? "").replace(/[&<>"']/g, (c) => ({
    "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;",
  }[c]));
}

function kindClass(kind) {
  return "kind-" + kind.replace(/\./g, "-");
}

const KIND_LABEL = {
  "session.start": "会话开始",
  "session.meta": "会话信息",
  "turn.start": "用户输入",
  "llm.response": "模型输出",
  "llm.reasoning": "推理",
  "tool.call": "工具调用",
  "tool.result": "工具结果",
  "llm.usage": "用量",
  "turn.end": "轮次结束",
  "session.end": "会话结束",
  "compaction": "上下文压缩",
  "error": "错误",
};

// ---- 数据加载 ----
async function loadHarnesses() {
  try {
    state.harnesses = await window.go.main.App.ListHarnesses();
    renderHarnesses();
  } catch (e) {
    setStatus("加载 harness 失败: " + e);
  }
}

async function selectHarness(name) {
  state.currentHarness = name;
  state.currentSession = null;
  state.events = [];
  renderHarnesses();
  renderTimeline();
  $("sessionPaneTitle").textContent = "会话 · " + name;
  try {
    state.sessions = await window.go.main.App.ListSessions(name);
    renderSessions();
  } catch (e) {
    setStatus("加载会话失败: " + e);
  }
}

async function selectSession(sessionId) {
  state.currentSession = sessionId;
  state.expanded.clear();
  state.replay = false;
  renderSessions();
  try {
    state.events = await window.go.main.App.GetEvents(state.currentHarness, sessionId);
    renderTimeline();
    setStatus(`已加载 ${state.events.length} 个步骤`);
  } catch (e) {
    setStatus("加载事件失败: " + e);
  }
}

// ---- 渲染：harness ----
function renderHarnesses() {
  const ul = $("harnessList");
  ul.innerHTML = "";
  for (const h of state.harnesses) {
    const li = document.createElement("li");
    li.className = "harness-item" + (h.name === state.currentHarness ? " selected" : "")
      + (h.eventCount === 0 ? " empty" : "");
    li.innerHTML = `
      <div class="harness-name"><span>${esc(h.name)}</span></div>
      <div class="harness-meta"><span>${h.sessionCount} 会话</span><span>${fmtNum(h.eventCount)} 事件</span></div>
      <div class="harness-meta"><span>${esc(h.lastTime || "—")}</span></div>`;
    li.onclick = () => selectHarness(h.name);
    ul.appendChild(li);
  }
}

function fmtNum(n) {
  if (n >= 10000) return (n / 10000).toFixed(1) + "万";
  return String(n);
}

// ---- 渲染：会话 ----
function renderSessions() {
  const ul = $("sessionList");
  ul.innerHTML = "";
  const filter = $("sessionFilter").value.trim().toLowerCase();
  const list = state.sessions.filter((s) =>
    !filter || s.sessionId.toLowerCase().includes(filter) || (s.title || "").toLowerCase().includes(filter));
  for (const s of list) {
    const li = document.createElement("li");
    li.className = "session-item" + (s.sessionId === state.currentSession ? " selected" : "");
    li.innerHTML = `
      <div class="session-title">${esc(s.title || s.sessionId)}</div>
      <div class="session-id">${esc(s.sessionId)}</div>
      <div class="session-meta"><span>${s.eventCount} 事件</span><span>${esc((s.lastTime || "").slice(5, 16))}</span></div>`;
    li.onclick = () => selectSession(s.sessionId);
    ul.appendChild(li);
  }
}

// ---- 渲染：时间线 ----
function renderTimeline() {
  const box = $("timeline");
  if (!state.currentSession) {
    box.innerHTML = `<div class="timeline-empty">选择左侧的 harness 和会话，查看每一步的完整内容</div>`;
    $("timelineTitle").textContent = "时间线";
    return;
  }
  $("timelineTitle").textContent =
    `${state.currentHarness} · ${truncate(state.currentSession, 28)} · ${state.events.length} 步`;

  const filtered = state.kindFilter
    ? state.events.filter((e) => e.kind === state.kindFilter)
    : state.events;

  box.innerHTML = "";
  if (filtered.length === 0) {
    box.innerHTML = `<div class="timeline-empty">无匹配步骤</div>`;
    return;
  }
  for (const e of filtered) {
    box.appendChild(renderStep(e));
  }
  if (state.replay) highlightReplay();
}

function renderStep(e) {
  const div = document.createElement("div");
  const isExpanded = state.expanded.has(e.seq);
  div.className = "step" + (isExpanded ? " expanded" : "");
  div.dataset.seq = e.seq;

  const preview = oneLine(e.content);
  const label = KIND_LABEL[e.kind] || e.kind;

  div.innerHTML = `
    <div class="step-left">
      <span class="step-time">${esc((e.time || "").slice(11) || (e.time || "").slice(0, 19))}</span>
      <span class="step-kind ${kindClass(e.kind)}">${esc(label)}</span>
    </div>
    <div class="step-body">
      <div class="step-title">${esc(e.title || label)}</div>
      ${!isExpanded && preview ? `<div class="step-preview">${esc(preview)}</div>` : ""}
      ${isExpanded ? renderExpanded(e) : ""}
    </div>`;

  div.onclick = (ev) => {
    if (ev.target.closest(".step-content")) return; // 内容区滚动不收起
    if (state.expanded.has(e.seq)) state.expanded.delete(e.seq);
    else state.expanded.add(e.seq);
    div.replaceWith(renderStep(e));
  };
  return div;
}

function renderExpanded(e) {
  const contentLen = (e.content || "").length;
  let html = "";
  if (contentLen > 0) {
    html += `<div class="step-content">${esc(e.content)}</div>`;
    html += `<div class="content-note">全文 ${fmtBytes(contentLen)} · 点击空白处收起</div>`;
  } else {
    html += `<div class="content-note">（此步骤无内容载荷）</div>`;
  }
  if (e.meta) {
    let pretty = e.meta;
    try { pretty = JSON.stringify(JSON.parse(e.meta), null, 2); } catch (_) {}
    html += `<div class="step-meta">${esc(pretty)}</div>`;
  }
  return html;
}

function fmtBytes(n) {
  if (n >= 1024) return (n / 1024).toFixed(1) + " KB";
  return n + " B";
}

function oneLine(s) {
  return String(s || "").replace(/\s+/g, " ").trim().slice(0, 300);
}

function truncate(s, n) {
  s = String(s || "");
  return s.length > n ? s.slice(0, n - 1) + "…" : s;
}

// ---- 搜索 ----
async function doSearch() {
  const q = $("searchInput").value.trim();
  if (!q) return;
  const scope = $("scopeCurrent").checked ? state.currentHarness : "";
  setStatus("搜索中…");
  try {
    const hits = await window.go.main.App.Search(scope, q);
    state.searchQuery = q;
    renderSearchResults(hits, q);
    $("searchPanel").classList.remove("hidden");
    setStatus(`搜索 "${q}"：${hits.length} 条命中`);
  } catch (e) {
    setStatus("搜索失败: " + e);
  }
}

function renderSearchResults(hits, q) {
  $("searchTitle").textContent = `搜索结果："${q}"（${hits.length} 条）`;
  const ul = $("searchResults");
  ul.innerHTML = "";
  if (hits.length === 0) {
    ul.innerHTML = `<li class="search-hit dim">无命中</li>`;
    return;
  }
  const ql = q.toLowerCase();
  for (const h of hits) {
    const li = document.createElement("li");
    li.className = "search-hit";
    const content = oneLine(h.content || h.title);
    const idx = content.toLowerCase().indexOf(ql);
    let marked = esc(content);
    if (idx >= 0) {
      marked = esc(content.slice(0, idx)) + "<mark>" + esc(content.slice(idx, idx + q.length)) + "</mark>" + esc(content.slice(idx + q.length));
    }
    li.innerHTML = `
      <div class="search-hit-head"><span class="h">${esc(h.harness)}</span><span>${esc(h.sessionId)}</span><span>#${h.seq}</span><span>${esc((h.time || "").slice(0, 19))}</span></div>
      <div class="search-hit-content">${marked}</div>`;
    li.onclick = async () => {
      $("searchPanel").classList.add("hidden");
      await selectHarness(h.harness);
      await selectSession(h.sessionId);
      // 展开并滚动到命中步骤
      state.expanded.add(h.seq);
      renderTimeline();
      const el = document.querySelector(`.step[data-seq="${h.seq}"]`);
      if (el) { el.scrollIntoView({ block: "center" }); el.classList.add("replay-current"); }
    };
    ul.appendChild(li);
  }
}

// ---- live 跟随 ----
async function toggleLive() {
  state.live = !state.live;
  $("btnLive").classList.toggle("active", state.live);
  $("liveIndicator").classList.toggle("hidden", !state.live);
  if (state.live) {
    setStatus("LIVE：每 3 秒增量采集");
    liveTick(); // 立即执行一轮
    state.liveTimer = setInterval(liveTick, 3000);
  } else {
    clearInterval(state.liveTimer);
    state.liveTimer = null;
    setStatus("LIVE 已关闭");
  }
}

async function liveTick() {
  try {
    const harness = state.currentHarness || "";
    await window.go.main.App.Collect(harness);
    await loadHarnesses();
    if (state.currentSession) {
      const before = state.events.length;
      state.events = await window.go.main.App.GetEvents(state.currentHarness, state.currentSession);
      if (state.events.length !== before) {
        renderTimeline();
        const tl = $("timeline");
        tl.scrollTop = tl.scrollHeight; // 自动滚到底部
      }
    } else if (state.currentHarness) {
      state.sessions = await window.go.main.App.ListSessions(state.currentHarness);
      renderSessions();
    }
    setStatus(`LIVE · 最后刷新 ${new Date().toLocaleTimeString()}`);
  } catch (e) {
    setStatus("LIVE 刷新失败: " + e);
  }
}

// ---- 回放 ----
function toggleReplay() {
  if (!state.currentSession) { setStatus("先选择会话再回放"); return; }
  state.replay = !state.replay;
  state.replayIndex = 0;
  $("btnReplay").classList.toggle("active", state.replay);
  if (state.replay) {
    setStatus("回放模式：→/n 下一步 · ←/p 上一步 · esc 退出");
    highlightReplay();
  } else {
    setStatus("已退出回放");
    renderTimeline();
  }
}

function highlightReplay() {
  document.querySelectorAll(".step.replay-current").forEach((el) => el.classList.remove("replay-current"));
  const events = state.kindFilter ? state.events.filter((e) => e.kind === state.kindFilter) : state.events;
  if (events.length === 0) return;
  const e = events[Math.min(state.replayIndex, events.length - 1)];
  state.expanded.add(e.seq);
  renderTimeline();
  const el = document.querySelector(`.step[data-seq="${e.seq}"]`);
  if (el) {
    el.classList.add("replay-current");
    el.scrollIntoView({ block: "center" });
  }
  setStatus(`回放 ${state.replayIndex + 1}/${events.length} · ${e.kind}`);
}

function replayStep(delta) {
  const events = state.kindFilter ? state.events.filter((e) => e.kind === state.kindFilter) : state.events;
  if (!state.replay || events.length === 0) return;
  state.replayIndex = Math.max(0, Math.min(events.length - 1, state.replayIndex + delta));
  highlightReplay();
}

// ---- 事件绑定 ----
function bind() {
  $("btnCollect").onclick = async () => {
    setStatus("增量采集中…");
    $("btnCollect").disabled = true;
    try {
      const res = await window.go.main.App.Collect("");
      const detail = Object.entries(res.details).map(([k, v]) => `${k} ${v}`).join(" · ");
      setStatus(`采集完成：${res.total} 条新事件（${detail}）`);
      await loadHarnesses();
      if (state.currentHarness) {
        state.sessions = await window.go.main.App.ListSessions(state.currentHarness);
        renderSessions();
      }
      if (state.currentSession) {
        state.events = await window.go.main.App.GetEvents(state.currentHarness, state.currentSession);
        renderTimeline();
      }
    } catch (e) {
      setStatus("采集失败: " + e);
    } finally {
      $("btnCollect").disabled = false;
    }
  };

  $("btnRefresh").onclick = async () => {
    await loadHarnesses();
    if (state.currentHarness) await selectHarness(state.currentHarness);
    setStatus("已刷新");
  };

  $("btnLive").onclick = toggleLive;
  $("btnReplay").onclick = toggleReplay;
  $("kindFilter").onchange = (e) => { state.kindFilter = e.target.value; renderTimeline(); };
  $("sessionFilter").oninput = renderSessions;
  $("searchInput").addEventListener("keydown", (e) => { if (e.key === "Enter") doSearch(); });
  $("btnCloseSearch").onclick = () => $("searchPanel").classList.add("hidden");

  $("btnAbout").onclick = async () => {
    try {
      $("aboutVersion").textContent = "版本：" + (await window.go.main.App.Version());
    } catch (_) {}
    $("aboutPanel").classList.remove("hidden");
  };
  $("btnCloseAbout").onclick = () => $("aboutPanel").classList.add("hidden");
  $("btnOpenData").onclick = () => window.go.main.App.OpenInExplorer().catch(() => {});

  document.addEventListener("keydown", (e) => {
    if (e.target.tagName === "INPUT" || e.target.tagName === "SELECT") return;
    if (e.key === "Escape") {
      if (state.replay) toggleReplay();
      else $("searchPanel").classList.add("hidden");
    }
    if (state.replay) {
      if (e.key === "ArrowRight" || e.key === "n") replayStep(1);
      if (e.key === "ArrowLeft" || e.key === "p") replayStep(-1);
    }
  });
}

// ---- 启动 ----
window.addEventListener("DOMContentLoaded", async () => {
  bind();
  await loadHarnesses();
  // 自动选中第一个有数据的 harness
  const first = state.harnesses.find((h) => h.eventCount > 0);
  if (first) await selectHarness(first.name);
  setStatus("就绪");
});
