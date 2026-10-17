<div align="center">

<img src="docs/assets/logo/kudzu-icon.svg" width="88" alt="Kudzu logo: a commit graph that grows a leaf">

# Kudzu

### Git 是给人用的，Kudzu 是给 Agent 用的。

**Agent 时代的代码基础设施。**<br/>
不用全量 clone，历史直接走 API，上千个 agent 同时提交也不用排队，原生支持 MCP。

[![release](https://img.shields.io/github/v/release/garyships/kudzu?color=3fb950)](https://github.com/garyships/kudzu/releases)
[![MCP: 16 tools](https://img.shields.io/badge/MCP-16_tools-8b5cf6)](docs/AI.md)
[![license: MIT](https://img.shields.io/badge/license-MIT-3fb950)](LICENSE)

[**官网**](https://garyships.github.io/kudzu/) · [**演示视频**](https://garyships.github.io/kudzu/#demo) · [设计文档](docs/DESIGN.zh-CN.md) · [给 AI agent 用](docs/AI.md) · [English](README.md)

<img src="docs/assets/demo.gif" alt="Kudzu 演示" width="860">

<sub>⭐ agent 的数量很快会超过管它们的程序员。如果你也觉得版本控制该为此做好准备，点个 star。</sub>

</div>

## Agent 正在把 git 压垮

每个程序员手下很快都会有 100 个 agent，它们 24 小时不停地 clone、提交、合并，而且是同时进行。git 是为"一个人、一台笔记本"设计的，第一个扛不住的就是它：

| | git 哪里会崩 | Kudzu 怎么解决 |
|---|---|---|
| 🌪️ **Clone 风暴** | 每个 agent 沙箱、每个 CI 任务都要拉整个仓库和全部历史 | 客户端只拉自己要用的对象，中间有多层缓存。50 个相同的冷请求，上游只收到 1 个 |
| 🚦 **合并大堵车** | 上千个 agent 抢着往 `main` 合并：merge queue 要加锁，push 被拒，所有人 rebase 再重试 | 服务端自动 rebase，用 compare-and-swap 更新 ref。不加锁、不排队、不用 push |
| 🔒 **历史锁在本地** | agent 想问一句"这行是谁改的"，得先 checkout 整个仓库 | `log`、`blame`、`diff` 都是服务端 API，还有 [16 个 MCP 工具](#-让你的-agent-直接操作版本控制) |
| 🪞 **镜像泛滥** | 几百个镜像，每个都是完整副本，每个都要 repack 和 gc | 对象不可变、按内容寻址，放在多层缓存后面。没有 packfile，也不用维护 |

**这不是假设。** Anthropic 的 monorepo 四个月里从每天约 2,500 次合并涨到 3 万多次，git 扛不住，他们只能自己造了 Scion，并[公开了设计](https://www.anthropic.com/engineering/redesigning-source-control)。Kudzu 把这套架构开源出来，人人可用。文章发布第二天，第一版就跑起来了。

## 60 秒上手

```bash
curl -fsSL https://raw.githubusercontent.com/garyships/kudzu/main/install.sh | sh
cd 任意git仓库 && kudzu up      # 导入历史，同时启动 server → cache → daemon 三层
```

另开一个终端：

```bash
export KUDZU_REMOTE=unix:///tmp/kudzu.sock
kudzu log -n 5 --oneline                          # 不用 checkout 就能查历史
kudzu blame README.md                             # blame 在服务端算
kudzu clone --sparse src $KUDZU_REMOTE /tmp/wc    # 只拉 src/
```

装了 Go 也可以用 `go install github.com/garyships/kudzu/cmd/kudzu@latest`。Windows 版在 [Releases 页面](https://github.com/garyships/kudzu/releases)下载。

## 🤖 让你的 agent 直接操作版本控制

Kudzu 自带 MCP server。一行命令接入 Claude Code，其他 MCP 客户端也一样：

```bash
claude mcp add kudzu -e KUDZU_REMOTE=unix:///tmp/kudzu.sock -- kudzu mcp
```

然后直接问：

> *"`services/api/handler.go` 里的重试逻辑最后是谁改的？那次提交还动了哪些文件？"*<br/>
> *"今天 `services/api` 下面合进来了什么？"*<br/>
> *"把 `VERSION` 里的版本号加一，合进 main。"* 不需要 checkout，也不需要 push：服务端会自动 rebase 并合进主干。

一共 16 个工具：11 个只读（log、blame、diff、cat、ls、show 等），5 个写操作（`kudzu_write` 直接提交文件内容并合进主干，还有 land、rebase 等）。

- 加 `--read-only` 会隐藏所有写操作工具。
- 能用 shell 的 agent 也可以装 [Agent Skill](skills/kudzu/SKILL.md)。
- 所有查询和提交命令都支持 `--json`。

Cursor、Claude Desktop 等客户端的配置见 [docs/AI.md](docs/AI.md)。

## Kudzu 和 git 对比

| | git | Kudzu |
|---|---|---|
| **clone** | 每个客户端都拿完整历史 | 只拉你要的 tree 和 blob，默认就是 sparse |
| **历史、blame、diff** | 在本地 clone 上计算 | 服务端 API，不需要 checkout |
| **提交** | 本地 commit，再 push | 服务端直接创建 changeset，没有 push 这一步 |
| **合并进主干** | merge queue，全局锁 | 无锁：服务端 rebase，再对 ref 做 CAS |
| **读扩展** | 很多 mirror，每个都存整个仓库 | 多层缓存，缓存的都是不可变对象 |
| **维护** | 每个副本都要 repack 和 gc | 不需要，因为没有 packfile |
| **离线** | ✅ 完全支持 | ❌ 必须连服务端，这是刻意的设计 |
| **存储** | delta 压缩的 pack，体积更小 | 每个 blob 单独存，没有 delta，体积更大 |

架构图和流程图见 [英文 README](README.md#how-it-works)，或者 [官网](https://garyships.github.io/kudzu/) 上的动画。

## 分开运行三层

`kudzu up` 把三层服务跑在一个进程里。正式部署时，三层各自独立运行：一个 server、每个区域一个 cache、每台机器一个 daemon。

<details>
<summary>把 server、区域缓存、节点 daemon 分开启动</summary>

```bash
kudzu import --store ./data --git /path/to/repo --as main     # 导入 git 历史，可以重复执行；main 只会快进，在 Kudzu 里 land 的改动不会被回退
kudzu export --store ./data --git /path/to/mirror.git --ref main   # 把 Kudzu 里 land 的内容同步回 git 镜像，也是增量的

kudzu server --store ./data --listen 127.0.0.1:7070 &
kudzu cache  --upstream 127.0.0.1:7070 --listen 127.0.0.1:7071 &
kudzu daemon --upstream 127.0.0.1:7071 --listen unix:///tmp/kudzu.sock &
export KUDZU_REMOTE=unix:///tmp/kudzu.sock

kudzu clone --sparse services/api $KUDZU_REMOTE wc && cd wc
kudzu commit -m "api: tweak" --land main  # 在服务端创建 changeset 并合进主干
kudzu write -r main -m "bump" --land main VERSION=@./VERSION   # 完全不用工作区
```

unix socket 默认只允许创建者自己连接（权限 0600），因为能连上的人就能 land 改动、移动 ref。想让多个用户共用一个 daemon，把 socket 放在不是所有人都可写的目录里，并加上 `--socket-mode 0660`（或 `0666`）。如果 socket 在 `/tmp` 这类所有人都可写的目录里、又属于别的用户，客户端会拒绝连接，因为任何用户都可能抢先创建它。

</details>

## 实测数据

测试对象是一个真实仓库最近的 529 个提交（3613 个文件），所有层都跑在同一台机器上，只看量级，不是严格的 benchmark：
- 导入耗时约 5 秒
- sparse clone（冷 daemon）0.30 秒
- 全量 clone（daemon 已经缓存过）0.46 秒
- 另一台机器的全量 clone（区域缓存已经缓存过）0.49 秒
- 对照：同一个仓库 `git clone` 要 2.07 秒

代价是存储更大：服务端占 79 MB，git 的 packfile 只有 26 MB。

## 现状

Kudzu 是一个可以运行的原型，测试用 `go test -race ./...`。导入结果和 `git ls-tree`、`git blame --first-parent` 逐一核对过，导出也能完整回到 git。已经做了：按目录的权限控制（`kudzu server --auth`）、基于 Postgres 和兼容 S3 存储的多台无状态服务端（`--meta`、`--objects`）、垃圾回收（`kudzu gc --store`）、按需获取、可以直接编辑和提交的 FUSE checkout（`kudzu mount --writable`，用 `-tags fuse` 编译）。还没做：
- Spanner 或 CockroachDB 元数据存储（Postgres 已经可以用）

## 许可证

MIT。独立开源项目，架构基于 Anthropic 公开的 Scion 设计文章 [Redesigning source control at Anthropic](https://www.anthropic.com/engineering/redesigning-source-control)。
