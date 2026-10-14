/* Kudzu landing page: theme, copy buttons, video fallback, hero vine,
   terminal typing, and the git-vs-kudzu animations. Vanilla JS. */
(function () {
  "use strict";
  var NS = "http://www.w3.org/2000/svg";
  var root = document.documentElement;
  var reduceMQ = window.matchMedia ? window.matchMedia("(prefers-reduced-motion: reduce)") : { matches: false };
  var darkMQ = window.matchMedia ? window.matchMedia("(prefers-color-scheme: dark)") : { matches: true };
  function reduced() { return reduceMQ.matches; }
  function $(sel, el) { return (el || document).querySelector(sel); }
  function $$(sel, el) { return Array.prototype.slice.call((el || document).querySelectorAll(sel)); }
  function sleep(ms) { return new Promise(function (r) { setTimeout(r, ms); }); }
  function onVisible(el, fn, threshold) {
    if (!el) return;
    if (!("IntersectionObserver" in window)) { fn(); return; }
    var io = new IntersectionObserver(function (es) {
      es.forEach(function (e) { if (e.isIntersecting) { io.disconnect(); fn(); } });
    }, { threshold: threshold || 0.25 });
    io.observe(el);
  }

  /* ---------- theme toggle ---------- */
  var themeBtn = $("#theme-toggle");
  function effectiveTheme() {
    var t = root.getAttribute("data-theme");
    return t === "light" || t === "dark" ? t : (darkMQ.matches ? "dark" : "light");
  }
  function syncThemeLabel() {
    if (themeBtn) themeBtn.setAttribute("aria-label", effectiveTheme() === "dark" ? "Switch to light theme" : "Switch to dark theme");
  }
  if (themeBtn) {
    syncThemeLabel();
    themeBtn.addEventListener("click", function () {
      var next = effectiveTheme() === "dark" ? "light" : "dark";
      root.setAttribute("data-theme", next);
      try { localStorage.setItem("kudzu-theme", next); } catch (e) { /* storage unavailable */ }
      syncThemeLabel();
    });
    if (darkMQ.addEventListener) darkMQ.addEventListener("change", syncThemeLabel);
  }

  /* ---------- nav border on scroll ---------- */
  var nav = $("#nav");
  function onScroll() { if (nav) nav.classList.toggle("scrolled", window.scrollY > 8); }
  window.addEventListener("scroll", onScroll, { passive: true });
  onScroll();

  /* ---------- reveal on scroll ---------- */
  var revealEls = $$("[data-reveal]");
  if ("IntersectionObserver" in window && !reduced()) {
    var rio = new IntersectionObserver(function (es) {
      es.forEach(function (e) { if (e.isIntersecting) { e.target.classList.add("in"); rio.unobserve(e.target); } });
    }, { threshold: 0.15, rootMargin: "0px 0px -40px 0px" });
    revealEls.forEach(function (el) { rio.observe(el); });
  } else {
    revealEls.forEach(function (el) { el.classList.add("in"); });
  }

  /* ---------- copy buttons ---------- */
  var live = document.createElement("div");
  live.className = "sr-only";
  live.setAttribute("aria-live", "polite");
  document.body.appendChild(live);
  function copyText(text) {
    if (navigator.clipboard && window.isSecureContext) return navigator.clipboard.writeText(text);
    return new Promise(function (res, rej) {
      var ta = document.createElement("textarea");
      ta.value = text; ta.setAttribute("readonly", ""); ta.style.position = "fixed"; ta.style.opacity = "0";
      document.body.appendChild(ta); ta.select();
      try { document.execCommand("copy") ? res() : rej(); } catch (e) { rej(e); }
      document.body.removeChild(ta);
    });
  }
  $$("[data-copy]").forEach(function (block) {
    var pre = $("pre", block);
    if (!pre) return;
    var btn = document.createElement("button");
    btn.type = "button";
    btn.className = "copy-btn";
    btn.setAttribute("aria-label", "Copy to clipboard");
    btn.innerHTML = '<svg class="i-copy" aria-hidden="true"><use href="#i-copy"/></svg><svg class="i-check" aria-hidden="true"><use href="#i-check"/></svg>';
    btn.addEventListener("click", function () {
      copyText(pre.textContent.replace(/\s+$/, "")).then(function () {
        btn.classList.add("copied");
        live.textContent = "Copied to clipboard";
        setTimeout(function () { btn.classList.remove("copied"); live.textContent = ""; }, 1600);
      }, function () { live.textContent = "Copy failed"; });
    });
    block.appendChild(btn);
  });

  /* ---------- tabs ---------- */
  $$('[role="tablist"]').forEach(function (list) {
    var tabs = $$('[role="tab"]', list);
    function select(tab) {
      tabs.forEach(function (t) {
        var on = t === tab;
        t.setAttribute("aria-selected", on ? "true" : "false");
        t.tabIndex = on ? 0 : -1;
        var panel = document.getElementById(t.getAttribute("aria-controls"));
        if (panel) panel.hidden = !on;
      });
    }
    tabs.forEach(function (t, i) {
      t.addEventListener("click", function () { select(t); });
      t.addEventListener("keydown", function (e) {
        var d = e.key === "ArrowRight" ? 1 : e.key === "ArrowLeft" ? -1 : 0;
        if (!d) return;
        e.preventDefault();
        var n = tabs[(i + d + tabs.length) % tabs.length];
        select(n); n.focus();
      });
    });
  });

  /* ---------- demo video: degrade gracefully if it is missing ---------- */
  var video = $("#demo-video"), fallback = $("#video-fallback"), frame = $("#video-frame");
  if (video && fallback) {
    var failed = false;
    var fail = function () {
      if (failed) return;
      failed = true;
      fallback.hidden = false;
      frame.classList.add("no-video");
      video.removeAttribute("controls");
      video.setAttribute("aria-hidden", "true");
      video.setAttribute("tabindex", "-1");
    };
    video.addEventListener("error", fail);
    if (video.error || video.networkState === 3) fail();
  }

  /* ---------- hero vines ---------- */
  function growVines() {
    var animate = !reduced();
    $$(".vine").forEach(function (svg, vi) {
      var layer = document.createElementNS(NS, "g");
      var stems = $$(".vine-stem", svg);
      var seed = 7 + vi * 13;
      function rnd() { seed = (seed * 9301 + 49297) % 233280; return seed / 233280; }
      stems.forEach(function (path, si) {
        var len = path.getTotalLength();
        var thin = path.classList.contains("thin");
        var delay = vi * 0.35 + (thin ? 1.3 + si * 0.25 : 0);
        var dur = thin ? 1.5 : 3.4;
        path.style.setProperty("--len", len.toFixed(1));
        path.style.setProperty("--dur", dur + "s");
        path.style.setProperty("--delay", delay + "s");
        var side = si % 2 ? 1 : -1;
        var d = thin ? 26 : 40;
        var k = 0;
        while (d < len - 8) {
          var p = path.getPointAtLength(d);
          var q = path.getPointAtLength(Math.min(len, d + 2));
          var ang = Math.atan2(q.y - p.y, q.x - p.x) * 180 / Math.PI;
          var t = delay + dur * Math.pow(d / len, 0.9) * 0.92;
          var size = (thin ? 22 : 30) + rnd() * 12;
          if (!thin && k % 3 === 1) {
            // a changeset node on the main vine
            var ng = document.createElementNS(NS, "g");
            ng.setAttribute("transform", "translate(" + p.x.toFixed(1) + " " + p.y.toFixed(1) + ")");
            var ring = document.createElementNS(NS, "circle");
            ring.setAttribute("r", "9"); ring.setAttribute("class", "node-ring pop");
            ring.style.setProperty("--ox", "50%"); ring.style.setProperty("--oy", "50%");
            ring.style.setProperty("--delay", t.toFixed(2) + "s");
            var dot = document.createElementNS(NS, "circle");
            dot.setAttribute("r", "4.5"); dot.setAttribute("class", "node pop");
            dot.style.setProperty("--ox", "50%"); dot.style.setProperty("--oy", "50%");
            dot.style.setProperty("--delay", t.toFixed(2) + "s");
            ng.appendChild(ring); ng.appendChild(dot);
            layer.appendChild(ng);
          } else {
            var lg = document.createElementNS(NS, "g");
            var rot = ang + 90 + side * (48 + rnd() * 22);
            lg.setAttribute("transform", "translate(" + p.x.toFixed(1) + " " + p.y.toFixed(1) + ") rotate(" + rot.toFixed(1) + ")");
            var use = document.createElementNS(NS, "use");
            var h = size * 54 / 48;
            use.setAttribute("href", "#i-leaf");
            use.setAttribute("x", (-size / 2).toFixed(1));
            use.setAttribute("y", (-h).toFixed(1));
            use.setAttribute("width", size.toFixed(1));
            use.setAttribute("height", h.toFixed(1));
            use.setAttribute("class", "leaf pop" + (rnd() > 0.55 ? " alt" : ""));
            use.style.setProperty("--delay", (t + 0.1).toFixed(2) + "s");
            lg.appendChild(use);
            layer.appendChild(lg);
            side = -side;
          }
          if (!thin && k % 4 === 2) {
            // a curling tendril
            var nx = -(q.y - p.y), ny = q.x - p.x, nl = Math.hypot(nx, ny) || 1;
            nx /= nl; ny /= nl;
            var R = 9 + rnd() * 5, sgn = side;
            var cx = p.x + nx * R * sgn, cy = p.y + ny * R * sgn;
            var a0 = Math.atan2(p.y - cy, p.x - cx), T = Math.PI * 2.3, pts = [];
            for (var s = 0; s <= 28; s++) {
              var u = s / 28, a = a0 + sgn * u * T, r = R * (1 - 0.8 * u);
              pts.push((cx + r * Math.cos(a)).toFixed(1) + " " + (cy + r * Math.sin(a)).toFixed(1));
            }
            var tp = document.createElementNS(NS, "path");
            tp.setAttribute("d", "M" + pts.join(" L"));
            tp.setAttribute("class", "tendril");
            tp.style.setProperty("--delay", (t + 0.2).toFixed(2) + "s");
            layer.appendChild(tp);
          }
          d += (thin ? 30 : 46) + rnd() * 16;
          k++;
        }
        if (thin) {
          // kudzu blossoms at the end of side shoots
          var end = path.getPointAtLength(len);
          var bg = document.createElementNS(NS, "g");
          bg.setAttribute("transform", "translate(" + end.x.toFixed(1) + " " + end.y.toFixed(1) + ")");
          for (var b = 0; b < 6; b++) {
            var c = document.createElementNS(NS, "circle");
            c.setAttribute("cx", (Math.cos(b * 1.1) * (3 + b * 1.2)).toFixed(1));
            c.setAttribute("cy", (b * 3.2 - 6).toFixed(1));
            c.setAttribute("r", (3.4 - b * 0.35).toFixed(1));
            c.setAttribute("class", "bloom pop");
            c.style.setProperty("--ox", "50%"); c.style.setProperty("--oy", "50%");
            c.style.setProperty("--delay", (delay + dur + 0.1 + b * 0.06).toFixed(2) + "s");
            bg.appendChild(c);
          }
          layer.appendChild(bg);
        }
      });
      svg.appendChild(layer);
      if (animate) svg.classList.add("grow");
    });
  }
  growVines();

  /* ---------- hero terminal typing ---------- */
  (function () {
    var term = $("#hero-terminal");
    if (!term || reduced()) return;
    var lines = $$(".t-line", term);
    var caret = $(".t-caret", term);
    var data = lines.map(function (l) {
      var ps = $(".t-ps", l);
      var text = ps ? l.textContent.slice(ps.textContent.length) : l.textContent;
      return { el: l, ps: ps, text: text, cmd: l.classList.contains("t-cmd") };
    });
    term.classList.add("typing");
    data.forEach(function (d) {
      if (d.cmd) {
        d.el.textContent = "";
        d.el.appendChild(d.ps);
        d.node = document.createTextNode("");
        d.el.appendChild(d.node);
      }
    });
    var stopped = false;
    function finish() {
      stopped = true;
      data.forEach(function (d) { if (d.cmd) d.node.nodeValue = d.text; d.el.classList.add("shown"); });
      lines[lines.length - 1].after(caret);
    }
    (async function () {
      await sleep(700);
      for (var i = 0; i < data.length && !stopped; i++) {
        var d = data[i];
        d.el.classList.add("shown");
        if (d.cmd) {
          d.el.appendChild(caret);
          for (var c = 1; c <= d.text.length && !stopped; c++) {
            d.node.nodeValue = d.text.slice(0, c);
            await sleep(16 + Math.random() * 22);
          }
          await sleep(380);
        } else if (d.el.classList.contains("t-out")) {
          d.el.after(caret);
          await sleep(260);
        } else {
          d.el.after(caret);
          await sleep(200);
        }
      }
      if (!stopped) lines[lines.length - 1].after(caret);
    })();
    term.addEventListener("click", function () { if (!stopped) finish(); });
  })();

  /* ---------- git vs kudzu: what lands on the client ---------- */
  (function () {
    var card = $("#clone-demo");
    if (!card) return;
    var COLS = 24, ROWS = 7;
    var WANT = { 0: "tree", 1: "blob", 6: "tree", 9: "tree", 10: "blob", 11: "blob", 12: "blob" };
    var grids = {};
    ["git", "kudzu"].forEach(function (name) {
      var g = $('[data-grid="' + name + '"]', card);
      var cells = [];
      for (var r = 0; r < ROWS; r++) {
        for (var c = 0; c < COLS; c++) {
          var el = document.createElement("span");
          el.className = "cell";
          g.appendChild(el);
          cells.push({ el: el, r: r, c: c });
        }
      }
      grids[name] = { cells: cells, bar: $(".cbar-fill", g.parentNode) };
    });
    var token = 0;
    function reset() {
      Object.keys(grids).forEach(function (k) {
        grids[k].cells.forEach(function (x) { x.el.className = "cell"; });
        grids[k].bar.style.transition = "none";
        grids[k].bar.style.width = "0";
      });
    }
    function finalState() {
      grids.git.cells.forEach(function (x) { x.el.classList.add("on"); });
      grids.kudzu.cells.forEach(function (x) { if (x.r === 0 && WANT[x.c]) x.el.className = "cell on " + WANT[x.c]; });
      grids.git.bar.style.width = "100%";
      grids.kudzu.bar.style.width = "100%";
    }
    function run() {
      var my = ++token;
      reset();
      if (reduced()) { finalState(); return; }
      void card.offsetWidth;
      var gitDur = 2600, kDur = 700;
      grids.git.bar.style.transition = "width " + gitDur + "ms linear";
      grids.kudzu.bar.style.transition = "width " + kDur + "ms linear";
      grids.git.bar.style.width = "100%";
      grids.kudzu.bar.style.width = "100%";
      // git: the whole packfile, oldest history first
      var order = grids.git.cells.slice().sort(function (a, b) { return (b.r - a.r) || (a.c - b.c); });
      order.forEach(function (x, i) {
        setTimeout(function () { if (my === token) x.el.classList.add("on"); }, (i / order.length) * gitDur);
      });
      // kudzu: just the wanted objects at one revision, trees first
      var wanted = grids.kudzu.cells.filter(function (x) { return x.r === 0 && WANT[x.c]; });
      wanted.sort(function (a, b) { return (WANT[a.c] === "tree" ? 0 : 1) - (WANT[b.c] === "tree" ? 0 : 1) || a.c - b.c; });
      wanted.forEach(function (x, i) {
        setTimeout(function () { if (my === token) x.el.className = "cell on " + WANT[x.c]; }, 80 + (i / wanted.length) * kDur);
      });
    }
    onVisible(card, run, 0.35);
    var btn = $('[data-replay="clone"]', card);
    if (btn) btn.addEventListener("click", run);
  })();

  /* ---------- git vs kudzu: two agents land at once ---------- */
  (function () {
    var card = $("#land-demo");
    if (!card) return;
    var token = 0;
    var X = { c3: 126, a: 172, b: 218, startA: 236, startB: 290, top: 46, mid: 86, main: 132, ref: 106 };
    var panes = {};
    ["queue", "kudzu"].forEach(function (name) {
      var svg = $('[data-land="' + name + '"]', card);
      panes[name] = {
        svg: svg,
        log: $('[data-log="' + name + '"]', card),
        ref: $(".ref", svg),
        lock: $(".lock", svg),
        lockLabel: $('[data-label="qlock"]', svg),
        a: $('[data-id="' + (name === "queue" ? "qa" : "ka") + '"]', svg),
        b: $('[data-id="' + (name === "queue" ? "qb" : "kb") + '"]', svg),
        wait: $('[data-label="qwait"]', svg),
        fail: $('[data-label="kfail"]', svg)
      };
    });
    function place(el, x, y, instant) {
      if (instant) el.style.transition = "none";
      el.style.transform = "translate(" + x + "px, " + y + "px)";
      if (instant) { void el.getBoundingClientRect(); el.style.transition = ""; }
    }
    function log(p, who, html) {
      var li = document.createElement("li");
      li.innerHTML = '<span class="who">' + who + "</span><span>" + html + "</span>";
      p.log.appendChild(li);
    }
    function lock(p, holder) {
      if (!p.lock) return;
      p.lock.setAttribute("class", holder ? "lock held" : "lock open");
      p.lockLabel.textContent = holder ? "lock: held by " + holder : "lock: free";
      p.lockLabel.classList.toggle("held", !!holder);
    }
    function reset() {
      Object.keys(panes).forEach(function (k) {
        var p = panes[k];
        p.log.innerHTML = "";
        place(p.ref, X.c3, X.ref, true);
        place(p.a, X.startA, X.top, true);
        place(p.b, X.startB, X.top, true);
        p.a.setAttribute("class", "cs mover");
        p.b.setAttribute("class", "cs mover");
        $("text", p.b).textContent = "b";
        if (p.lock) { p.lock.setAttribute("class", "lock"); p.lockLabel.textContent = "lock: free"; p.lockLabel.classList.remove("held"); }
        if (p.wait) p.wait.classList.remove("show");
        if (p.fail) p.fail.classList.remove("show");
      });
    }
    function finalState() {
      var q = panes.queue, k = panes.kudzu;
      place(q.a, X.a, X.main, true); place(q.b, X.b, X.main, true); place(q.ref, X.b, X.ref, true);
      q.a.classList.add("landed"); q.b.classList.add("landed");
      log(q, "A", 'acquire lock <span class="ok">✓</span>');
      log(q, "B", '<span class="dim">waiting for lock…</span>');
      log(q, "A", "merge onto c3, release lock");
      log(q, "B", 'acquire lock <span class="ok">✓</span>');
      log(q, "B", "merge onto a, release lock");
      place(k.a, X.a, X.main, true); place(k.b, X.b, X.main, true); place(k.ref, X.b, X.ref, true);
      k.a.classList.add("landed"); k.b.classList.add("landed");
      $("text", k.b).textContent = "b′";
      log(k, "A", 'CAS main c3→a <span class="ok">✓</span>');
      log(k, "B", 'CAS main c3→b <span class="bad">✗ tip moved</span>');
      log(k, "B", 'rebase b onto a → b′ <span class="dim">(on the server)</span>');
      log(k, "B", 'CAS main a→b′ <span class="ok">✓ 2 attempts</span>');
    }
    async function run() {
      var my = ++token;
      reset();
      if (reduced()) { finalState(); return; }
      var q = panes.queue, k = panes.kudzu;
      async function step(ms) { await sleep(ms); if (my !== token) throw "stale"; }
      try {
        await step(500);
        // Both agents ask to land at the same moment.
        lock(q, "A");
        place(q.a, X.a, X.mid);
        log(q, "A", 'acquire lock <span class="ok">✓</span>');
        q.b.classList.add("wait"); q.wait.classList.add("show");
        log(q, "B", '<span class="dim">waiting for lock…</span>');

        place(k.a, X.a, X.main);
        place(k.b, X.a + 30, X.mid);
        await step(750);
        k.a.classList.add("landed"); place(k.ref, X.a, X.ref);
        log(k, "A", 'CAS main c3→a <span class="ok">✓</span>');
        await step(350);
        k.b.classList.add("fail"); k.fail.classList.add("show");
        log(k, "B", 'CAS main c3→b <span class="bad">✗ tip moved</span>');
        await step(700);
        k.fail.classList.remove("show"); k.b.classList.remove("fail");
        $("text", k.b).textContent = "b′";
        place(k.b, X.b, X.mid);
        log(k, "B", 'rebase b onto a → b′ <span class="dim">(on the server)</span>');
        await step(650);
        place(k.b, X.b, X.main);
        await step(500);
        k.b.classList.add("landed"); place(k.ref, X.b, X.ref);
        log(k, "B", 'CAS main a→b′ <span class="ok">✓ 2 attempts</span>');

        // Meanwhile the queue is still busy with A.
        await step(300);
        place(q.a, X.a, X.main);
        await step(700);
        q.a.classList.add("landed"); place(q.ref, X.a, X.ref);
        log(q, "A", "merge onto c3, release lock");
        lock(q, null);
        await step(500);
        lock(q, "B");
        q.b.classList.remove("wait"); q.wait.classList.remove("show");
        place(q.b, X.b, X.mid);
        log(q, "B", 'acquire lock <span class="ok">✓</span>');
        await step(1100);
        place(q.b, X.b, X.main);
        await step(700);
        q.b.classList.add("landed"); place(q.ref, X.b, X.ref);
        lock(q, null);
        log(q, "B", "merge onto a, release lock");
      } catch (e) { if (e !== "stale") throw e; }
    }
    onVisible(card, run, 0.4);
    var btn = $('[data-replay="land"]', card);
    if (btn) btn.addEventListener("click", run);
    reset();
  })();

  /* ---------- under the hood: a living changeset log ---------- */
  (function () {
    var list = $("#log-list"), derived = $$("#hood-derived .dd");
    if (!list || !derived.length || reduced()) return;
    var msgs = [
      ["api: retry on 503", "1 file"], ["web: lazy-load charts", "3 files"], ["tools: bump codegen", "2 files"],
      ["api: add rate limiter", "4 files"], ["docs: fix typo", "1 file"], ["ci: shard test suite", "2 files"]
    ];
    var seq = 1043, i = 0, timer = null, n = 0;
    function tick() {
      seq++;
      var m = msgs[i++ % msgs.length];
      var li = document.createElement("li");
      li.className = "fresh";
      li.innerHTML = '<span class="log-seq">' + seq + '</span><span class="log-msg"></span><span class="log-files">' + m[1] + "</span>";
      li.querySelector(".log-msg").textContent = m[0];
      list.appendChild(li);
      while (list.children.length > 3) list.removeChild(list.firstElementChild);
      // eager types are derived on append; blame waits until someone asks
      derived.forEach(function (dd, j) {
        if (dd.classList.contains("eager")) {
          setTimeout(function () { dd.classList.add("tick"); setTimeout(function () { dd.classList.remove("tick"); }, 700); }, 250 + j * 160);
        }
      });
      if (++n % 3 === 0) {
        var lazy = derived[derived.length - 1];
        setTimeout(function () { lazy.classList.add("tick"); setTimeout(function () { lazy.classList.remove("tick"); }, 900); }, 1400);
      }
    }
    var hood = $("#hood");
    if ("IntersectionObserver" in window) {
      new IntersectionObserver(function (es) {
        es.forEach(function (e) {
          if (e.isIntersecting && !timer) { tick(); timer = setInterval(tick, 2800); }
          else if (!e.isIntersecting && timer) { clearInterval(timer); timer = null; }
        });
      }, { threshold: 0.3 }).observe(hood);
    }
  })();
})();


/* ---------- live GitHub star count (best effort, hidden on failure) ---------- */
(function () {
  var els = document.querySelectorAll("[data-stars]");
  if (!els.length || !window.fetch) return;
  fetch("https://api.github.com/repos/garyships/kudzu", { headers: { Accept: "application/vnd.github+json" } })
    .then(function (r) { return r.ok ? r.json() : null; })
    .then(function (repo) {
      if (!repo || typeof repo.stargazers_count !== "number" || repo.stargazers_count < 10) return;
      var n = repo.stargazers_count;
      var text = n >= 1000 ? (n / 1000).toFixed(n >= 10000 ? 0 : 1) + "k" : String(n);
      els.forEach(function (el) { el.textContent = "★ " + text; el.hidden = false; });
    })
    .catch(function () {});
})();
