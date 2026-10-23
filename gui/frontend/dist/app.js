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
  explains: [], // 与 events 一一对应的自然语言说明
  usage: null, // 当前会话的用量/成本合计（后端 internal/usage 口径）
  kindFilter: "",
  live: false,
  liveTimer: null,
  replay: false,
  replayIndex: 0,
  expanded: new Set(), // 已展开步骤的 seq
  searchQuery: "",
  view: "monitor", // monitor | compare
  metaCache: new Map(), // seq -> 解析后的 meta 对象（失败标记等要按 meta 判定，避免重复 JSON.parse）
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
  const prevSession = state.currentSession;
  state.currentSession = null;
  state.events = [];
  state.usage = null;
  renderHarnesses();
  renderTimeline();
  $("sessionPaneTitle").textContent = "会话 · " + name;
  try {
    state.sessions = await window.go.main.App.ListSessions(name);
    renderSessions();
    // 自动选中：若之前选的会话还在本 harness 则保留；否则选第一个（最近活动），时间线立即有内容。
    const keep = prevSession && state.sessions.some((s) => s.sessionId === prevSession);
    if (keep) {
      await selectSession(prevSession);
    } else if (state.sessions.length > 0) {
      await selectSession(state.sessions[0].sessionId);
    }
  } catch (e) {
    setStatus("加载会话失败: " + e);
  }
}

async function selectSession(sessionId) {
  state.currentSession = sessionId;
  state.expanded.clear();
  state.metaCache.clear();
  state.replay = false;
  renderSessions();
  try {
    state.events = await window.go.main.App.GetEvents(state.currentHarness, sessionId);
    // 自然语言说明（与 events 一一对应；失败不阻塞时间线展示）
    try {
      state.explains = await window.go.main.App.ExplainSession(state.currentHarness, sessionId);
    } catch (_) {
      state.explains = [];
    }
    // 会话用量/成本合计（各家口径不同，合计规则在后端 internal/usage；失败不阻塞展示）
    try {
      state.usage = await window.go.main.App.SessionUsage(state.currentHarness, sessionId);
    } catch (_) {
      state.usage = null;
    }
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

// fmtTokens 千分位显示 token 数（用量动辄几万，可读性靠分隔符）。
function fmtTokens(n) {
  return Number(n || 0).toLocaleString("en-US");
}

// fmtCost 显示费用；后端标注「费用未知」时不显示成 $0（那是「不知道」，不是「免费」）。
function fmtCost(cost, known) {
  if (!known) return "";
  if (cost > 0 && cost < 0.0001) return "<$0.0001";
  return "$" + cost.toFixed(4);
}

// renderUsageBar 在时间线顶部显示当前会话的用量/成本合计。
//
// 各家 harness 的记录形态不同（codex 的 token_count 带累计块与增量块、hermes 是
// 「会话×模型」汇总、claude 是每会话一条 cost-state、pi/magpie 是逐次调用），
// 归一与合计规则全在后端 internal/usage，前端只负责展示与如实标注口径。
function renderUsageBar() {
  const bar = $("usageBar");
  const u = state.usage;
  if (!state.currentSession || !u || (!u.records && !u.skipped)) {
    bar.classList.add("hidden");
    bar.innerHTML = "";
    return;
  }
  bar.classList.remove("hidden");

  const item = (label, value, title) =>
    `<span class="usage-item"${title ? ` title="${esc(title)}"` : ""}><span class="usage-label">${esc(label)}</span>${esc(value)}</span>`;

  const parts = [];
  // 输入的口径各家不同：codex 的 input_tokens 本身含缓存命中，其余家的缓存读是
  // 单列的另一项。后端已把这两点标在 summary 上（inputIncludesCache /
  // cacheInTotal），这里照实标注，别让用户以为「输入」一律不含缓存。
  const inputTitle = u.inputIncludesCache
    ? "输入 token（含缓存命中部分——codex 的输入字段本身就是含缓存的）"
    : "输入 token（不含缓存读取）";
  parts.push(item("输入", fmtTokens(u.inputTokens), inputTitle));
  parts.push(item("输出", fmtTokens(u.outputTokens), "输出 token"));
  if (u.cacheRead) parts.push(item("缓存读", fmtTokens(u.cacheReadTokens), "从缓存读取的 token（计费通常更低）"));
  if (u.cacheWrite) parts.push(item("缓存写", fmtTokens(u.cacheWriteTokens), "写入缓存的 token"));
  if (u.reasoning) parts.push(item("推理", fmtTokens(u.reasoningTokens), "思考/推理 token"));
  parts.push(`<span class="usage-total"${u.cacheInTotal ? ' title="合计已包含缓存读取的 token"' : ' title="合计不含缓存读取（缓存读是上面单列的一项）"'}>合计 ${esc(fmtTokens(u.totalTokens))} tokens</span>`);
  if (u.costKnown) {
    parts.push(`<span class="usage-cost">${esc(fmtCost(u.costUsd, true))}</span>`);
  } else {
    parts.push(`<span class="usage-cost dim" title="该 harness 的记录里没有费用字段（如 magpie 只记 token 与状态）">费用未知</span>`);
  }
  if (u.failedCalls) {
    parts.push(item("失败调用", String(u.failedCalls), "非 200 的模型调用（限流 429 / 超时 499 等）"));
  }

  // 口径说明：codex 的 token_count 带两个块，必须取「本次调用增量」而不是累计计数器——
  // 用户看到说明才知道这个数字是怎么来的（也解释了为什么 codex 的输入远大于输出）。
  const notes = {
    codex: ["逐次调用相加", "该 harness 的 token_count 每条记录带「本次调用」与「文件累计」两个块；这里取本次调用逐条相加（累计块会随会话恢复继承上游基数，不可相加）"],
    hermes: ["按模型汇总相加", "每条记录是「会话 × 模型」的合计行，相加即会话总用量"],
    claude: ["会话汇总", "每个会话一条 cost-state 记录，含分模型明细"],
    pi: ["逐次调用相加", "每条记录是一次模型调用的用量"],
    magpie: ["逐次调用相加", "每条记录是一次模型调用的用量（含状态，可看限流/失败）"],
  };
  const [noteLabel, noteTitle] = notes[state.currentHarness] || ["逐条相加", "每条记录相加"];
  const note = `<span class="usage-note" title="${esc(noteTitle)}">${esc(noteLabel)}</span>`;

  let models = "";
  if (u.models && u.models.length > 1) {
    const rows = u.models
      .map((m) => `<span class="usage-model">${esc(m.model)} <span class="dim">${esc(fmtTokens(m.totalTokens))}</span></span>`)
      .join("");
    models = `<div class="usage-models" title="分模型用量">${rows}</div>`;
  }

  const recNote = u.skipped
    ? ` · ${u.skipped} 条记录无法解析（meta 为空/损坏），未计入`
    : "";

  bar.innerHTML = `<div class="usage-line">${parts.join("")}${note}</div>${models}
    <div class="usage-sub">${u.records} 条用量记录${esc(recNote)}</div>`;
}

// ---- 渲染：会话 ----
//
// 会话项上带一个状态标记（等待你 / 失败 / 进行中 / 已完成 / 空闲）——
// 这是竞品（tmux-agent-sidebar 的状态列、MioIsland 的待批准提醒）都有、
// 而我们此前没有的东西：不用点进时间线就知道哪个会话卡住了。
const STATE_LABEL = {
  waiting: "等待你",
  failed: "失败",
  done: "已完成",
  active: "进行中",
  idle: "空闲",
};

function stateBadge(s) {
  if (!s.state) return "";
  const label = s.stateLabel || STATE_LABEL[s.state] || "";
  if (!label) return "";
  const tip = s.pendingQuestion
    ? `${s.stateReason || ""}\n${s.pendingQuestion}`
    : (s.stateReason || "");
  return `<span class="state-badge state-${esc(s.state)}" title="${esc(tip)}">${esc(label)}</span>`;
}

function renderSessions() {
  const ul = $("sessionList");
  ul.innerHTML = "";
  const filter = $("sessionFilter").value.trim().toLowerCase();
  const list = state.sessions.filter((s) =>
    !filter || s.sessionId.toLowerCase().includes(filter) || (s.title || "").toLowerCase().includes(filter));
  // 需要你出手的会话排最前——它们是最该被看到的。
  const rank = { waiting: 0, failed: 1, active: 2, done: 3, idle: 4 };
  list.sort((a, b) => {
    const ra = rank[a.state] === undefined ? 5 : rank[a.state];
    const rb = rank[b.state] === undefined ? 5 : rank[b.state];
    if (ra !== rb) return ra - rb;
    return (b.lastTime || "").localeCompare(a.lastTime || "");
  });
  for (const s of list) {
    const li = document.createElement("li");
    li.className = "session-item" + (s.sessionId === state.currentSession ? " selected" : "") + (s.pinned ? " pinned" : "");
    li.innerHTML = `
      <button class="pin-btn" title="${s.pinned ? "取消置顶" : "置顶会话"}">${s.pinned ? "📌" : "·"}</button>
      <div class="session-body">
        <div class="session-title">${stateBadge(s)}<span class="session-title-text">${esc(s.title || s.sessionId)}</span></div>
        <div class="session-id">${esc(s.sessionId)}</div>
        <div class="session-meta"><span>${s.eventCount} 事件</span><span>${esc((s.lastTime || "").slice(5, 16))}</span></div>
      </div>`;
    // 📌 切换置顶；失败恢复原状态并提示（网络/库错误不应静默丢失用户操作）
    li.querySelector(".pin-btn").onclick = async (ev) => {
      ev.stopPropagation();
      try {
        await window.go.main.App.SetSessionPinned(state.currentHarness, s.sessionId, !s.pinned);
        state.sessions = await window.go.main.App.ListSessions(state.currentHarness);
        renderSessions();
      } catch (e) {
        setStatus("置顶失败: " + e);
      }
    };
    li.onclick = () => selectSession(s.sessionId);
    ul.appendChild(li);
  }
}

// ---- 渲染：时间线 ----
function renderTimeline() {
  const box = $("timeline");
  renderUsageBar();
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
  const ctx = {
    events: state.events,
    explains: state.explains,
    expanded: state.expanded,
  };
  for (const e of filtered) {
    box.appendChild(renderStep(e, ctx));
  }
  if (state.replay) highlightReplay();
}

// parseMeta 解析步骤的 meta JSON（带缓存）；解析失败返回 {}。
// 失败标记（is_error / exit_code / error.type）都藏在 meta 里，渲染时按它判定。
function parseMeta(e, ctx) {
  const cache = (ctx && ctx.metaCache) || state.metaCache;
  if (cache.has(e.seq)) return cache.get(e.seq);
  let obj = {};
  if (e.meta) {
    try { obj = JSON.parse(e.meta) || {}; } catch (_) { obj = {}; }
  }
  cache.set(e.seq, obj);
  return obj;
}

// stepIssue 判断一个步骤是否是「失败/异常」，返回徽标文字或 ""。
//
// 两个失败信号（都来自采集器写入 meta 的结构化字段，不做正文猜测）：
//   kind=error           —— 采集器已判定失败（回合被中断 / 接口失败 / 模型调用失败）
//   meta.is_error=true   —— 工具调用失败（hermes 退出码非 0、pi isError）
function stepIssue(e, ctx) {
  if (e.kind === "error") return "错误";
  const m = parseMeta(e, ctx);
  if (m.is_error === true) {
    return m.exit_code !== undefined && m.exit_code !== null ? `失败·退出码 ${m.exit_code}` : "失败";
  }
  if (e.kind === "tool.call" || e.kind === "tool.result") {
    if (typeof m.status === "number" && m.status !== 0 && m.status !== 200) return `失败·HTTP ${m.status}`;
  }
  return "";
}

// renderStep 渲染一个步骤卡片。ctx = {events, explains, expanded}——主监控与对比实验台
// 共用同一套样式与交互（展开/收起），只是各自持有独立的事件流与展开集合。
function renderStep(e, ctx) {
  const div = document.createElement("div");
  const isExpanded = ctx.expanded.has(e.seq);
  div.className = "step" + (isExpanded ? " expanded" : "");
  div.dataset.seq = e.seq;

  const preview = oneLine(e.content);
  const label = KIND_LABEL[e.kind] || e.kind;
  const idx = ctx.events.findIndex((x) => x.seq === e.seq);
  const ex = ctx.explains[idx];

  // 工具往返徽标：同一个工具的多次调用靠「第 N 轮」区分（配对键 tooltip 可核对）。
  let roundBadge = "";
  if (ex && ex.round > 0) {
    roundBadge = `<span class="step-round" title="第 ${ex.round} 轮工具往返 · 配对键 ${esc(ex.toolKey || "")}">第${ex.round}轮</span>`;
  }

  // 失败徽标：失败的步骤此前与成功的长得一模一样（工具失败只体现在正文里），
  // 这里按 meta 的结构化标记在卡片上直接标出来，扫一眼就能发现哪一步断了。
  const issue = stepIssue(e, ctx);
  const issueBadge = issue
    ? `<span class="step-issue" title="这一步失败/异常（依据 meta 的 is_error / exit_code / type 字段）">${esc(issue)}</span>`
    : "";

  div.innerHTML = `
    <div class="step-left">
      <span class="step-time">${esc((e.time || "").slice(11) || (e.time || "").slice(0, 19))}</span>
      <span class="step-kind ${kindClass(e.kind)}">${esc(label)}</span>
      ${roundBadge}
      ${issueBadge}
    </div>
    <div class="step-body">
      <div class="step-title">${esc(e.title || label)}</div>
      ${!isExpanded && preview ? `<div class="step-preview">${esc(preview)}</div>` : ""}
      ${isExpanded ? renderExpanded(e, idx, ctx) : ""}
    </div>`;

  div.onclick = (ev) => {
    if (ev.target.closest(".step-content") || ev.target.closest(".step-explain")) return; // 内容区滚动不收起
    if (ctx.expanded.has(e.seq)) ctx.expanded.delete(e.seq);
    else ctx.expanded.add(e.seq);
    div.replaceWith(renderStep(e, ctx));
  };
  return div;
}

// renderLinkRow 渲染「这次调用 → 哪一步返回」的跳转行（相隔很远的往返靠它对齐）。
function renderLinkRow(targetSeq, label, note) {
  return `<div class="explain-row"><span class="explain-label">${esc(label)}</span>
    <span><a href="#" class="step-link" data-jump="${targetSeq}">第 ${targetSeq} 步 ↗</a>${note ? ` <span class="link-note">${esc(note)}</span>` : ""}</span></div>`;
}

function renderExpanded(e, idx, ctx) {
  const contentLen = (e.content || "").length;
  let html = "";
  // 自然语言说明（用户需求：每一步讲清楚衔接关系）
  const ex = ctx.explains[idx];
  if (ex) {
    html += `<div class="step-explain">`;
    if (ex.round > 0) {
      html += `<div class="explain-row"><span class="explain-label">工具往返</span><span>第 ${ex.round} 轮</span></div>`;
    }
    if (e.kind === "tool.call" && ex.resultSeq > 0) {
      html += renderLinkRow(ex.resultSeq, "返回在哪", ex.waitNote);
    } else if (e.kind === "tool.call" && ex.round > 0 && !ex.resultSeq) {
      html += `<div class="explain-row"><span class="explain-label">返回在哪</span><span class="link-note">${esc(ex.waitNote || "尚无返回结果")}</span></div>`;
    }
    if (e.kind === "tool.result" && ex.callSeq > 0) {
      html += renderLinkRow(ex.callSeq, "调用在哪", ex.waitNote);
    }
    html += `<div class="explain-row"><span class="explain-label">这一步</span><span>${esc(ex.summary)}</span></div>
      <div class="explain-row"><span class="explain-label">输入来自</span><span>${esc(ex.from)}</span></div>
      <div class="explain-row"><span class="explain-label">输出去向</span><span>${esc(ex.to)}</span></div>
      <div class="explain-row"><span class="explain-label">给用户看吗</span><span>${esc(ex.visible)}</span></div>
      <div class="explain-row"><span class="explain-label">为什么这样流转</span><span>${esc(ex.why)}</span></div>
      <div class="explain-row"><span class="explain-label">衔接</span><span>${esc(ex.nextHint)}</span></div>`;
    if (e.kind === "tool.result" && !ex.callSeq && ex.waitNote) {
      html += `<div class="explain-row"><span class="explain-label">注意</span><span class="link-note">${esc(ex.waitNote)}</span></div>`;
    }
    html += `</div>`;
  }
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

// jumpToStep 在时间线内展开并滚动到某一步（工具往返的调用 ↔ 返回互跳）。
// rerender 由调用方给出（主监控 = renderTimeline；对比实验台 = renderCompareColumns），
// scope 限定查找范围（对比实验台里同一 seq 在四列中都可能出现）。
function jumpToStep(seq, ctx, rerender, scope) {
  ctx.expanded.add(seq);
  rerender();
  const root = scope || document;
  const el = root.querySelector(`.step[data-seq="${seq}"]`);
  if (el) {
    el.scrollIntoView({ block: "center" });
    el.classList.add("replay-current");
    setTimeout(() => el.classList.remove("replay-current"), 1600);
  } else {
    setStatus(`第 ${seq} 步不在当前视图里（可能被类型过滤或已滚动出列表）`);
  }
}

// onStepLinkClick 处理说明里的「第 N 步 ↗」跳转（主监控与对比实验台共用）。
function onStepLinkClick(ev) {
  const a = ev.target.closest(".step-link");
  if (!a) return;
  ev.preventDefault();
  ev.stopPropagation();
  const seq = Number(a.dataset.jump);
  if (!seq) return;
  const col = a.closest(".compare-col");
  if (col) {
    const harness = col.dataset.harness;
    const c = compare.cols[harness];
    if (c) jumpToStep(seq, c, renderCompareColumns, col);
    return;
  }
  jumpToStep(seq, { expanded: state.expanded }, renderTimeline, $("timeline"));
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
    }
    // 会话列表要一起刷新：状态标记（等待你 / 失败）是会变的——
    // agent 跑完一轮就从「进行中」变成「已完成」，也可能中途等你回答。
    // 不刷新的话，标记会停在打开页面那一刻的样子。
    if (state.currentHarness) {
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

// ---- 视图切换 ----
// 两个视图都是 show/hide（不引入路由）。切到对比实验台时刷新历史批次。
function switchView(v) {
  state.view = v;
  $("monitorView").classList.toggle("hidden", v !== "monitor");
  $("compareView").classList.toggle("hidden", v !== "compare");
  $("btnViewMonitor").classList.toggle("active", v === "monitor");
  $("btnViewCompare").classList.toggle("active", v === "compare");
  if (v === "compare") {
    loadRuns();
  } else {
    stopComparePoll();
  }
}

// ============================================================================
// 对比实验台
// ============================================================================

// 四家 harness 的规范顺序与展示名（与 internal/runner.All 一致）。
const COMPARE_HARNESSES = [
  { name: "hermes", label: "Hermes" },
  { name: "codex", label: "Codex" },
  { name: "claude", label: "Claude Code" },
  { name: "pi", label: "pi" },
];

const STATUS_LABEL = {
  pending: "等待中",
  running: "运行中",
  done: "完成",
  failed: "失败",
  timeout: "超时",
  stopped: "已停止",
};

// compare 是对比实验台的视图状态（与主监控 state 分离）。
const compare = {
  runID: null,        // 当前查看的批次 id
  detail: null,       // GetRun 结果
  runs: [],           // ListRuns 结果
  cols: {},           // harness -> { events, explains, expanded:Set }
  timer: null,        // 兜底轮询定时器（运行中批次）
  pendingReload: false,
};

function initCompareHarnesses() {
  const box = $("compareHarnesses");
  box.innerHTML = "";
  for (const h of COMPARE_HARNESSES) {
    const id = "chk-" + h.name;
    const label = document.createElement("label");
    label.className = "compare-harness";
    label.innerHTML = `<input type="checkbox" id="${id}" value="${h.name}" checked /> ${esc(h.label)}`;
    box.appendChild(label);
  }
}

function selectedHarnesses() {
  return COMPARE_HARNESSES
    .map((h) => h.name)
    .filter((n) => $(`chk-${n}`) && $(`chk-${n}`).checked);
}

// startCompare 启动一次对比：创建批次后立即返回 runID，之后靠事件 + 轮询拉增量。
async function startCompare() {
  const prompt = $("comparePrompt").value.trim();
  const harnesses = selectedHarnesses();
  if (!prompt) { setStatus("请输入要对比的问题"); return; }
  if (harnesses.length === 0) { setStatus("至少勾选一家 harness"); return; }

  $("btnStartCompare").disabled = true;
  setStatus("启动对比…");
  try {
    const runID = await window.go.main.App.StartCompare(prompt, harnesses);
    compare.runID = runID;
    compare.detail = null;
    compare.cols = {};
    setStatus(`批次 #${runID} 已启动（${harnesses.join(" / ")}）`);
    await loadRuns(runID);
    await loadRun(runID);
    startComparePoll();
  } catch (e) {
    setStatus("启动失败: " + e);
  } finally {
    $("btnStartCompare").disabled = false;
  }
}

async function stopRun() {
  if (compare.runID == null) return;
  $("btnStopRun").disabled = true;
  setStatus("停止中…");
  try {
    await window.go.main.App.StopRun(compare.runID);
    setStatus(`批次 #${compare.runID} 已请求停止`);
    await loadRun(compare.runID);
    await loadRuns(compare.runID);
  } catch (e) {
    setStatus("停止失败: " + e);
  }
}

// loadRuns 拉历史批次列表（倒序），填 select；keepID 指定要选中的批次。
async function loadRuns(keepID) {
  try {
    compare.runs = await window.go.main.App.ListRuns();
  } catch (e) {
    setStatus("加载批次失败: " + e);
    return;
  }
  const sel = $("runSelect");
  sel.innerHTML = "";
  if (compare.runs.length === 0) {
    const opt = document.createElement("option");
    opt.value = "";
    opt.textContent = "（暂无批次）";
    sel.appendChild(opt);
    return;
  }
  for (const r of compare.runs) {
    const opt = document.createElement("option");
    opt.value = String(r.id);
    const st = r.status === "running" ? "运行中" : "已完成";
    opt.textContent = `#${r.id} · ${st} · ${(r.createdAt || "").slice(5, 16)} · ${truncate(r.prompt, 34)}`;
    sel.appendChild(opt);
  }
  const want = keepID != null ? keepID : compare.runID;
  if (want != null && compare.runs.some((r) => r.id === want)) {
    sel.value = String(want);
  } else {
    sel.value = String(compare.runs[0].id);
  }
}

// loadRun 拉批次详情 + 各家事件流，重建并列列区。
async function loadRun(runID) {
  if (runID == null) return;
  compare.runID = runID;
  let detail;
  try {
    detail = await window.go.main.App.GetRun(runID);
  } catch (e) {
    setStatus("加载批次详情失败: " + e);
    return;
  }
  compare.detail = detail;

  // 保留已展开步骤（刷新时不打断阅读）。
  const prevCols = compare.cols;
  compare.cols = {};
  for (const r of detail.runners || []) {
    const prev = prevCols[r.harness];
    compare.cols[r.harness] = {
      events: prev ? prev.events : [],
      explains: prev ? prev.explains : [],
      expanded: prev ? prev.expanded : new Set(),
      metaCache: prev ? prev.metaCache : new Map(),
    };
  }

  // 并发拉四家事件流（只拉状态非 pending 的——pending 还没有事件）。
  await Promise.all((detail.runners || []).map(async (r) => {
    if (r.status === "pending") return;
    try {
      const [events, explains] = await Promise.all([
        window.go.main.App.GetRunEvents(runID, r.harness),
        window.go.main.App.ExplainRunEvents(runID, r.harness).catch(() => []),
      ]);
      compare.cols[r.harness].events = events;
      compare.cols[r.harness].explains = explains;
    } catch (_) { /* 单家失败不阻塞其余列 */ }
  }));

  renderCompareColumns();
  $("runMeta").textContent =
    `批次 #${detail.id} · ${detail.status === "running" ? "运行中" : "已完成"} · ${(detail.createdAt || "").slice(0, 19)}`;
  $("btnStopRun").disabled = !detail.active;
}

function renderCompareColumns() {
  const box = $("compareColumns");
  const d = compare.detail;
  box.innerHTML = "";
  if (!d) {
    box.innerHTML = `<div class="compare-empty">选择四家 harness，输入问题，点击「开始对比」——四列会并行实时更新</div>`;
    return;
  }
  const runners = d.runners || [];
  if (runners.length === 0) {
    box.innerHTML = `<div class="compare-empty">该批次没有 harness 记录</div>`;
    return;
  }
  for (const r of runners) {
    box.appendChild(renderCompareColumn(r));
  }
}

// renderCompareColumn 渲染一列：列头（名 + 状态徽标 + 耗时 + tokens）→ 步骤卡片 → 最终回答。
function renderCompareColumn(r) {
  const col = document.createElement("div");
  col.className = "compare-col status-" + (r.status || "pending");
  col.dataset.harness = r.harness;

  const st = STATUS_LABEL[r.status] || r.status || "未知";
  const dur = r.durationMs > 0 ? (r.durationMs / 1000).toFixed(1) + "s" : "—";
  const tok = (r.tokensIn || r.tokensOut)
    ? `${fmtNum(r.tokensIn)}/${fmtNum(r.tokensOut)}`
    : "—";
  const cost = r.costUsd > 0 ? `$${r.costUsd.toFixed(4)}` : "";

  const head = document.createElement("div");
  head.className = "compare-col-head";
  head.innerHTML = `
    <div class="compare-col-title">
      <span class="compare-col-name">${esc(r.harness)}</span>
      <span class="compare-badge status-${esc(r.status)}">${esc(st)}</span>
    </div>
    <div class="compare-col-stats">
      <span title="耗时">⏱ ${esc(dur)}</span>
      <span title="tokens 输入/输出">⇅ ${esc(tok)}</span>
      ${cost ? `<span title="费用">${esc(cost)}</span>` : ""}
    </div>
    ${r.sessionId ? `<div class="compare-col-session"><a href="#" class="session-link">会话 ${esc(truncate(r.sessionId, 18))} ↗</a></div>` : ""}`;
  col.appendChild(head);

  const link = head.querySelector(".session-link");
  if (link) {
    link.onclick = (ev) => {
      ev.preventDefault();
      jumpToSession(r.harness, r.sessionId);
    };
  }

  // 步骤列表
  const body = document.createElement("div");
  body.className = "compare-col-body";
  const c = compare.cols[r.harness];
  const events = c ? c.events : [];
  if (events.length === 0) {
    body.innerHTML = `<div class="compare-col-empty">${r.status === "pending" ? "等待启动…" : "暂无步骤"}</div>`;
  } else {
    const ctx = { events, explains: c.explains, expanded: c.expanded };
    for (const e of events) {
      body.appendChild(renderStep(e, ctx));
    }
  }
  col.appendChild(body);

  // 最终回答 / 状态说明。注意：error 事件（如 codex 的「忽略未知配置项」告警）不等于失败，
  // 只有 status 才是判据——所以这里按 status 决定标题，不按 error 文本是否存在。
  const foot = document.createElement("div");
  foot.className = "compare-col-foot";
  if (r.finalText) {
    foot.innerHTML = `<div class="compare-final-label">最终回答</div>
      <div class="compare-final-text">${esc(r.finalText)}</div>`;
  } else if (r.error) {
    const lbl = r.status === "stopped" ? "已停止"
      : r.status === "timeout" ? "超时"
      : r.status === "failed" ? "失败原因" : "说明";
    foot.innerHTML = `<div class="compare-final-label">${esc(lbl)}</div>
      <div class="${r.status === "failed" ? "compare-final-error" : "compare-final-note"}">${esc(r.error)}</div>`;
  } else {
    foot.innerHTML = `<div class="compare-col-empty dim">${
      r.status === "running" ? "运行中…" : r.status === "pending" ? "等待启动…" : "无最终回答"}</div>`;
  }
  col.appendChild(foot);

  return col;
}

// jumpToSession 点击会话链接 → 切回主监控视图并选中该会话（会话不在主库时提示）。
async function jumpToSession(harness, sessionId) {
  switchView("monitor");
  try {
    await selectHarness(harness);
    if (state.sessions.some((s) => s.sessionId === sessionId)) {
      await selectSession(sessionId);
    } else {
      setStatus(`会话 ${sessionId} 尚未采集到主监控库（对比批次的事件只存在批次里）`);
    }
  } catch (e) {
    setStatus("跳转失败: " + e);
  }
}

// startComparePoll 运行中批次的兜底刷新：事件推送 + 每 2 秒轮询（事件可能丢，轮询保底）。
function startComparePoll() {
  stopComparePoll();
  compare.timer = setInterval(async () => {
    if (compare.runID == null) return;
    if (compare.pendingReload) return; // 上一轮还没回来
    compare.pendingReload = true;
    try {
      await loadRun(compare.runID);
      await loadRuns(compare.runID);
      if (compare.detail && !compare.detail.active) stopComparePoll(); // 全部终态 → 停轮询
    } catch (_) {
    } finally {
      compare.pendingReload = false;
    }
  }, 2000);
}

function stopComparePoll() {
  if (compare.timer) {
    clearInterval(compare.timer);
    compare.timer = null;
  }
}

// onRunUpdate 由 Go 侧 runtime.EventsEmit("run:update", runID) 触发。
function onRunUpdate(runID) {
  if (state.view !== "compare") return;
  // runID 从 Go int64 过来，JS 侧按 number 处理（两侧比较用 ==，兼容字符串化）。
  if (compare.runID != null && Number(runID) !== Number(compare.runID)) return;
  loadRun(Number(runID)).then(() => loadRuns(Number(runID))).catch(() => {});
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
  // 工具往返跳转（「第 N 步 ↗」）：事件委托，主监控与对比实验台共用。
  $("monitorView").addEventListener("click", onStepLinkClick);
  $("compareColumns").addEventListener("click", onStepLinkClick);
  $("sessionFilter").oninput = renderSessions;
  $("searchInput").addEventListener("keydown", (e) => { if (e.key === "Enter") doSearch(); });
  $("btnCloseSearch").onclick = () => $("searchPanel").classList.add("hidden");

  // 视图切换
  $("btnViewMonitor").onclick = () => switchView("monitor");
  $("btnViewCompare").onclick = () => switchView("compare");

  // 对比实验台
  $("btnStartCompare").onclick = startCompare;
  $("btnStopRun").onclick = stopRun;
  $("btnReloadRuns").onclick = () => loadRuns(compare.runID);
  $("runSelect").onchange = (e) => {
    const id = Number(e.target.value);
    if (id) loadRun(id);
  };

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
  initCompareHarnesses();
  // 事件驱动刷新：Go 侧编排器每次落库 → runtime.EventsEmit("run:update", runID)。
  if (window.runtime && window.runtime.EventsOn) {
    window.runtime.EventsOn("run:update", onRunUpdate);
  }
  await loadHarnesses();
  // 自动选中第一个有数据的 harness
  const first = state.harnesses.find((h) => h.eventCount > 0);
  if (first) await selectHarness(first.name);
  setStatus("就绪");
});
