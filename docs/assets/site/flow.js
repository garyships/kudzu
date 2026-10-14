/* Kudzu landing page: the tiered want-based fetch visualization.
   A tiny simulation drives it: each tier keeps a set of objects, wants travel
   up only as far as the first tier that has the object, and objects flow back
   down and get cached on the way. Vanilla JS, no dependencies. */
(function () {
  "use strict";
  var stage = document.getElementById("flow-stage");
  if (!stage) return;

  var NS = "http://www.w3.org/2000/svg";
  var flow = document.getElementById("flow");
  var wiresSvg = document.getElementById("flow-wires");
  var pkSvg = document.getElementById("flow-packets");
  var stepsEl = document.getElementById("flow-steps");
  var stepItems = Array.prototype.slice.call(stepsEl.querySelectorAll("li"));
  var caption = document.getElementById("flow-caption");
  var toggleBtn = document.getElementById("flow-toggle");
  var replayBtn = document.getElementById("flow-replay");
  var wcTree = document.getElementById("wc-tree");
  var reduceMQ = window.matchMedia ? window.matchMedia("(prefers-reduced-motion: reduce)") : { matches: false };

  /* ---------- the repository ---------- */
  var OBJS = [
    { id: "t-root", kind: "tree" },
    { id: "t-services", kind: "tree" },
    { id: "t-docs", kind: "tree", skip: true },
    { id: "t-tools", kind: "tree", skip: true },
    { id: "t-api", kind: "tree" },
    { id: "t-web", kind: "tree", skip: true },
    { id: "b-readme", kind: "blob" },
    { id: "b-handler", kind: "blob" },
    { id: "b-routes", kind: "blob" },
    { id: "b-test", kind: "blob" },
    { id: "b-guide", kind: "blob", skip: true },
    { id: "b-gen", kind: "blob", skip: true },
    { id: "b-app", kind: "blob", skip: true },
    { id: "n-new", kind: "new" }
  ];
  var KIND = {};
  OBJS.forEach(function (o) { KIND[o.id] = o.kind; });
  var LEVELS = [["t-root"], ["t-services"], ["t-api"], ["b-readme", "b-handler", "b-routes", "b-test"]];
  var TIERS = ["server", "cache", "dA", "dB"];
  var AGENTS = ["a1", "a2", "a3", "b1", "b2", "b3"];
  var PARENT = { cache: "server", dA: "cache", dB: "cache", a1: "dA", a2: "dA", a3: "dA", b1: "dB", b2: "dB", b3: "dB" };

  var nodes = {};
  Array.prototype.forEach.call(stage.querySelectorAll("[data-node]"), function (el) { nodes[el.getAttribute("data-node")] = el; });
  var badges = {};
  Array.prototype.forEach.call(stage.querySelectorAll("[data-badge]"), function (el) { badges[el.getAttribute("data-badge")] = el; });
  var statEls = {};
  Array.prototype.forEach.call(stage.querySelectorAll("[data-stat]"), function (el) { statEls[el.getAttribute("data-stat")] = el; });
  var wcRows = {};
  Array.prototype.forEach.call(wcTree.querySelectorAll("[data-obj]"), function (el) { wcRows[el.getAttribute("data-obj")] = el; });

  // Build object slots inside every tier card.
  var slotEls = {};
  TIERS.forEach(function (t) {
    var box = stage.querySelector('[data-slots="' + t + '"]');
    slotEls[t] = {};
    OBJS.forEach(function (o) {
      var s = document.createElement("span");
      s.className = "slot " + o.kind;
      s.title = o.id.replace(/^[tbn]-/, "");
      box.appendChild(s);
      slotEls[t][o.id] = s;
    });
  });

  /* ---------- geometry: wires between tiers ---------- */
  var edges = {}; // key "upper>lower" -> path element
  var edgeUse = {};
  function edgeKey(upper, lower) { return upper + ">" + lower; }
  Object.keys(PARENT).forEach(function (child) {
    var k = edgeKey(PARENT[child], child);
    var p = document.createElementNS(NS, "path");
    p.setAttribute("class", "wire");
    wiresSvg.appendChild(p);
    edges[k] = p;
    edgeUse[k] = 0;
  });

  function layout() {
    var sr = stage.getBoundingClientRect();
    var w = Math.max(1, sr.width), h = Math.max(1, sr.height);
    [wiresSvg, pkSvg].forEach(function (svg) { svg.setAttribute("viewBox", "0 0 " + w + " " + h); });
    Object.keys(PARENT).forEach(function (child) {
      var up = nodes[PARENT[child]].getBoundingClientRect();
      var lo = nodes[child].getBoundingClientRect();
      var sx = up.left + up.width / 2 - sr.left, sy = up.bottom - sr.top;
      var ex = lo.left + lo.width / 2 - sr.left, ey = lo.top - sr.top;
      var dy = (ey - sy) * 0.55;
      edges[edgeKey(PARENT[child], child)].setAttribute("d",
        "M" + sx.toFixed(1) + " " + sy.toFixed(1) +
        " C" + sx.toFixed(1) + " " + (sy + dy).toFixed(1) + " " + ex.toFixed(1) + " " + (ey - dy).toFixed(1) + " " + ex.toFixed(1) + " " + ey.toFixed(1));
    });
  }
  layout();
  if (window.ResizeObserver) new ResizeObserver(layout).observe(stage);
  else window.addEventListener("resize", layout);
  if (document.fonts && document.fonts.ready) document.fonts.ready.then(layout);

  /* ---------- clock, timers, packets ---------- */
  var clock = { t: 0, last: null };
  var userPaused = false, visible = false, started = false, finished = false;
  var timers = [], packets = [], raf = null, gen = 0;
  var STOP = {};

  function paused() { return userPaused || !visible; }
  function frame(now) {
    raf = null;
    if (clock.last !== null && !paused()) clock.t += Math.min(now - clock.last, 64);
    clock.last = now;
    var i;
    for (i = timers.length - 1; i >= 0; i--) {
      if (timers[i].at <= clock.t) { var tm = timers.splice(i, 1)[0]; tm.resolve(); }
    }
    for (i = packets.length - 1; i >= 0; i--) {
      var p = packets[i];
      var u = (clock.t - p.start) / p.dur;
      if (u < 0) { p.el.style.opacity = "0"; continue; }
      if (!p.begun) { p.begun = true; useEdge(p.edge, 1); }
      p.el.style.opacity = "1";
      if (u >= 1) { finishPacket(p); packets.splice(i, 1); continue; }
      var e = u < 0.5 ? 4 * u * u * u : 1 - Math.pow(-2 * u + 2, 3) / 2;
      var path = edges[p.edge];
      var L = path.getTotalLength();
      var pt = path.getPointAtLength(p.dir === "down" ? e * L : (1 - e) * L);
      p.el.setAttribute("transform", "translate(" + pt.x.toFixed(1) + " " + pt.y.toFixed(1) + ")");
    }
    if ((timers.length || packets.length) && !paused()) raf = requestAnimationFrame(frame);
  }
  function kick() { if (!raf) { clock.last = null; raf = requestAnimationFrame(frame); } }
  function finishPacket(p) {
    if (p.begun) useEdge(p.edge, -1);
    if (p.el.parentNode) p.el.parentNode.removeChild(p.el);
    p.resolve();
  }
  function useEdge(k, d) {
    edgeUse[k] = Math.max(0, edgeUse[k] + d);
    edges[k].classList.toggle("active", edgeUse[k] > 0);
  }

  function makePacket(kind) {
    var g = document.createElementNS(NS, "g");
    var el;
    if (kind === "want") {
      el = document.createElementNS(NS, "circle");
      el.setAttribute("r", "4.5");
      el.setAttribute("class", "pk-want");
      g.appendChild(el);
    } else {
      var glow = document.createElementNS(NS, "circle");
      glow.setAttribute("r", "9");
      glow.setAttribute("class", "pk-glow pk-" + kind);
      g.appendChild(glow);
      if (kind === "tree") {
        el = document.createElementNS(NS, "rect");
        el.setAttribute("x", "-4.5"); el.setAttribute("y", "-4.5");
        el.setAttribute("width", "9"); el.setAttribute("height", "9"); el.setAttribute("rx", "2");
      } else {
        el = document.createElementNS(NS, "circle");
        el.setAttribute("r", kind === "new" ? "5.5" : "4.8");
      }
      el.setAttribute("class", "pk-" + kind);
      g.appendChild(el);
    }
    g.style.opacity = "0";
    pkSvg.appendChild(g);
    return g;
  }

  /* ---------- simulation state ---------- */
  var store, stats;
  function resetState() {
    store = { server: {}, cache: {}, dA: {}, dB: {} };
    OBJS.forEach(function (o) { if (o.kind !== "new") store.server[o.id] = true; });
    stats = { "server-req": 0, "cache-hit": 0, "cache-up": 0, "dA-hit": 0, "dA-up": 0, "dB-hit": 0, "dB-up": 0 };
    TIERS.forEach(function (t) {
      OBJS.forEach(function (o) { slotEls[t][o.id].className = "slot " + o.kind + (store[t][o.id] ? " full" : ""); });
    });
    Object.keys(stats).forEach(function (k) { statEls[k].textContent = "0"; statEls[k].classList.remove("bump"); });
    Object.keys(wcRows).forEach(function (k) { wcRows[k].className = ""; });
    Object.keys(badges).forEach(function (k) { badges[k].className = "badge"; badges[k].textContent = ""; });
    Object.keys(nodes).forEach(function (k) { nodes[k].classList.remove("hit", "miss", "coalesce", "active", "done"); });
    stepItems.forEach(function (li) { li.classList.remove("active", "done"); });
  }

  /* ---------- a run: everything awaits through ctx so replay can cancel ---------- */
  function Run(instant) {
    var g = ++gen;
    var self = this;
    this.instant = instant;
    this.hop = 520;
    function check() { if (g !== gen) throw STOP; }
    this.wait = function (ms) {
      if (self.instant) { check(); return Promise.resolve(); }
      return new Promise(function (res) { timers.push({ at: clock.t + ms, resolve: res }); kick(); }).then(check);
    };
    this.send = function (upper, lower, dir, kinds, hop) {
      if (self.instant || !kinds.length) { check(); return Promise.resolve(); }
      var k = edgeKey(upper, lower);
      var dur = hop || self.hop;
      var stagger = Math.min(90, dur / 5);
      var ps = kinds.map(function (kind, i) {
        return new Promise(function (res) {
          packets.push({ el: makePacket(kind), edge: k, dir: dir, start: clock.t + i * stagger, dur: dur, resolve: res, begun: false });
        });
      });
      kick();
      return Promise.all(ps).then(check);
    };
    this.all = function (ps) { return Promise.all(ps).then(check); };
  }

  function bump(key, n) {
    if (!n) return;
    stats[key] += n;
    var el = statEls[key];
    el.textContent = String(stats[key]);
    el.classList.remove("bump"); void el.offsetWidth; el.classList.add("bump");
  }
  var badgeTimers = {};
  function badge(run, tier, cls, text, ms) {
    if (run.instant) return;
    var b = badges[tier], n = nodes[tier];
    b.className = "badge show " + cls;
    b.textContent = text;
    n.classList.remove("hit", "miss", "coalesce");
    n.classList.add(cls === "b-hit" ? "hit" : cls === "b-co" || cls === "b-new" ? "coalesce" : "miss");
    clearTimeout(badgeTimers[tier]);
    badgeTimers[tier] = setTimeout(function () {
      b.className = "badge";
      n.classList.remove("hit", "miss", "coalesce");
    }, ms || 1300);
  }
  function storeAdd(tier, ids) {
    ids.forEach(function (id) {
      store[tier][id] = true;
      var s = slotEls[tier][id];
      s.classList.add("full");
      s.classList.remove("skip");
      s.classList.add("flash");
      setTimeout(function () { s.classList.remove("flash"); }, 260);
    });
  }
  function markSkipped(ids) {
    ids.forEach(function (id) {
      ["cache", "dA", "dB"].forEach(function (t) { if (!store[t][id]) slotEls[t][id].classList.add("skip"); });
      if (wcRows[id]) wcRows[id].className = "skip";
    });
  }
  function wc(agent, ids, state) {
    if (agent !== "a1") return;
    ids.forEach(function (id) { if (wcRows[id]) wcRows[id].className = state; });
  }
  function setActive(agentList, on) {
    agentList.forEach(function (a) { nodes[a].classList.toggle("active", on); });
  }

  // One want: an agent asks for `ids`; the batch climbs until a tier has
  // everything that is still missing, then flows back down, cached on the way.
  function want(run, agent, ids, hop) {
    var route = [agent];
    var n = agent;
    while (PARENT[n]) { n = PARENT[n]; route.push(n); }
    var missAt = [ids];
    var need = ids;
    var top = 1;
    wc(agent, ids, "want");
    var p = run.send(route[1], route[0], "up", need.map(function () { return "want"; }), hop);
    function climb(i) {
      var tier = route[i];
      var have = need.filter(function (id) { return store[tier][id]; });
      var miss = need.filter(function (id) { return !store[tier][id]; });
      if (tier === "server") {
        bump("server-req", need.length);
        badge(run, "server", "b-miss", "SERVE", 1100);
        top = i;
        return Promise.resolve();
      }
      bump(tier + "-hit", have.length);
      bump(tier + "-up", miss.length);
      if (!miss.length) {
        badge(run, tier, "b-hit", have.length > 1 ? "HIT ×" + have.length : "HIT", 1100);
        top = i;
        return Promise.resolve();
      }
      badge(run, tier, "b-miss", "MISS", 1100);
      missAt.push(miss);
      need = miss;
      return run.send(route[i + 1], tier, "up", miss.map(function () { return "want"; }), hop).then(function () { return climb(i + 1); });
    }
    return p.then(function () { return climb(1); }).then(function () {
      function descend(i) {
        if (i < 1) return Promise.resolve();
        var carry = missAt[i - 1];
        return run.send(route[i], route[i - 1], "down", carry.map(function (id) { return KIND[id]; }), hop).then(function () {
          if (i - 1 >= 1) storeAdd(route[i - 1], carry);
          else wc(agent, carry, "got");
          return descend(i - 1);
        });
      }
      return descend(top);
    });
  }

  function cloneSparse(run, agent, hop, pause) {
    var chain = Promise.resolve();
    LEVELS.forEach(function (lvl, i) {
      chain = chain.then(function () { return want(run, agent, lvl, hop); }).then(function () {
        return i < LEVELS.length - 1 ? run.wait(pause || 120) : null;
      });
    });
    return chain;
  }

  /* ---------- the script ---------- */
  var STEPS = [
    function (run) { // 1. root tree
      setActive(["a1"], true);
      return want(run, "a1", LEVELS[0], 600);
    },
    function (run) { // 2. services/, skip docs/ tools/
      markSkipped(["t-docs", "t-tools", "b-guide", "b-gen"]);
      return run.wait(500).then(function () { return want(run, "a1", LEVELS[1], 460); });
    },
    function (run) { // 3. api/, skip web/
      markSkipped(["t-web", "b-app"]);
      return run.wait(400).then(function () { return want(run, "a1", LEVELS[2], 460); });
    },
    function (run) { // 4. blobs in one batch
      return want(run, "a1", LEVELS[3], 480).then(function () {
        setActive(["a1"], false);
        nodes.a1.classList.add("done");
      });
    },
    function (run) { // 5. same machine
      setActive(["a2"], true);
      return cloneSparse(run, "a2", 300, 80).then(function () {
        setActive(["a2"], false);
        nodes.a2.classList.add("done");
      });
    },
    function (run) { // 6. new machine, warm region
      setActive(["b1"], true);
      return cloneSparse(run, "b1", 330, 80).then(function () {
        setActive(["b1"], false);
        nodes.b1.classList.add("done");
      });
    },
    function (run) { // 7. a change lands; everyone wants it; misses coalesce
      store.server["n-new"] = true;
      var s = slotEls.server["n-new"];
      s.classList.add("full", "new");
      badge(run, "server", "b-new", "NEW CHANGESET", 1500);
      var hop = 560;
      return run.wait(900).then(function () {
        setActive(AGENTS, true);
        return run.all(AGENTS.map(function (a) { return run.send(PARENT[a], a, "up", ["want"], hop); }));
      }).then(function () {
        badge(run, "dA", "b-co", "×3 → 1", 1600);
        badge(run, "dB", "b-co", "×3 → 1", 1600);
        bump("dA-up", 1); bump("dB-up", 1);
        return run.all([run.send("cache", "dA", "up", ["want"], hop), run.send("cache", "dB", "up", ["want"], hop)]);
      }).then(function () {
        badge(run, "cache", "b-co", "×2 → 1", 1600);
        bump("cache-up", 1);
        return run.send("server", "cache", "up", ["want"], hop);
      }).then(function () {
        bump("server-req", 1);
        badge(run, "server", "b-miss", "SERVE ×1", 1300);
        return run.send("server", "cache", "down", ["new"], hop);
      }).then(function () {
        storeAdd("cache", ["n-new"]);
        return run.all([run.send("cache", "dA", "down", ["new"], hop), run.send("cache", "dB", "down", ["new"], hop)]);
      }).then(function () {
        storeAdd("dA", ["n-new"]); storeAdd("dB", ["n-new"]);
        return run.all(AGENTS.map(function (a) { return run.send(PARENT[a], a, "down", ["new"], hop); }));
      }).then(function () {
        setActive(AGENTS, false);
        AGENTS.forEach(function (a) { nodes[a].classList.add("done"); });
      });
    }
  ];

  function showStep(i) {
    stepItems.forEach(function (li, j) {
      li.classList.toggle("active", j === i);
      li.classList.toggle("done", j < i);
    });
    var t = stepItems[i].querySelector(".fs-t").textContent;
    caption.querySelector(".flow-caption-n").textContent = String(i + 1);
    caption.querySelector(".flow-caption-t").textContent = t;
  }

  function staticBadges() {
    [["dA", "b-co", "×3 → 1"], ["dB", "b-co", "×3 → 1"], ["cache", "b-co", "×2 → 1"]].forEach(function (b) {
      badges[b[0]].className = "badge show " + b[1];
      badges[b[0]].textContent = b[2];
    });
  }

  function setToggle(state) { // "playing" | "paused" | "done"
    var label = toggleBtn.querySelector("span");
    toggleBtn.disabled = state === "done";
    toggleBtn.setAttribute("aria-pressed", state === "paused" ? "true" : "false");
    label.textContent = state === "paused" ? "Resume" : "Pause";
  }

  function clearMotion() {
    gen++;
    timers.splice(0).forEach(function (t) { t.resolve(); });
    packets.splice(0).forEach(function (p) { finishPacket(p); });
    Object.keys(edgeUse).forEach(function (k) { edgeUse[k] = 0; edges[k].classList.remove("active"); });
    Object.keys(badgeTimers).forEach(function (k) { clearTimeout(badgeTimers[k]); });
  }

  // Play from step `from`: earlier steps are applied instantly.
  function play(from) {
    clearMotion();
    resetState();
    finished = false;
    started = true;
    var fast = new Run(true);
    var chain = Promise.resolve();
    for (var i = 0; i < from; i++) {
      (function (j) { chain = chain.then(function () { return STEPS[j](fast); }); })(i);
    }
    var animate = !reduceMQ.matches;
    chain = chain.then(function () {
      var run = new Run(!animate);
      var c = Promise.resolve();
      for (var j = from; j < STEPS.length; j++) {
        (function (k) {
          c = c.then(function () {
            showStep(k);
            return run.wait(k === from ? 350 : 900);
          }).then(function () { return STEPS[k](run); });
        })(j);
      }
      return c.then(function () {
        return run.wait(400);
      }).then(function () {
        finished = true;
        stepItems.forEach(function (li) { li.classList.add("done"); });
        if (!animate) {
          staticBadges();
          stepItems.forEach(function (li) { li.classList.remove("active"); });
        }
        setToggle("done");
      });
    });
    if (animate) { userPaused = false; setToggle("playing"); }
    chain.catch(function (e) { if (e !== STOP) throw e; });
  }

  function renderStatic() {
    flow.classList.add("static");
    play(0);
  }

  /* ---------- controls ---------- */
  toggleBtn.addEventListener("click", function () {
    if (finished) return;
    userPaused = !userPaused;
    setToggle(userPaused ? "paused" : "playing");
    kick();
  });
  replayBtn.addEventListener("click", function () { play(0); kick(); });
  stepItems.forEach(function (li, i) {
    li.querySelector("button").addEventListener("click", function () {
      if (reduceMQ.matches) return;
      play(i);
      kick();
    });
  });

  if (reduceMQ.matches) {
    renderStatic();
  } else {
    resetState();
    showStep(0);
    setToggle("playing");
    if ("IntersectionObserver" in window) {
      new IntersectionObserver(function (entries) {
        entries.forEach(function (en) {
          visible = en.isIntersecting;
          if (visible && !started) play(0);
          if (visible) kick();
        });
      }, { threshold: 0.3 }).observe(stage);
    } else {
      visible = true;
      play(0);
    }
  }
  if (reduceMQ.addEventListener) {
    reduceMQ.addEventListener("change", function () { if (reduceMQ.matches) { clearMotion(); renderStatic(); } });
  }
  document.addEventListener("visibilitychange", function () { if (!document.hidden) kick(); });
})();
