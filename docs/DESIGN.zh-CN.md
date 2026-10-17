# Kudzu 设计说明

这份文档说明 Kudzu 怎样实现文章里描述的各个机制，以及文章没讲、实现时自行选择的地方。代码位置都相对于仓库根目录。

## 1. 数据模型

### changeset（`internal/object/changeset.go`）

工作的基本单位是 changeset，也就是一条写前日志（WAL）记录。它和 git commit 最大的区别是**不指向 root tree**，而是列出相对**第一个父节点**改了哪些文件：

```json
{
  "parents": ["<id>"],
  "author": "Alice <a@example.com>", "time": 1760000000, "tz": 480,
  "message": "api: tweak handler",
  "changes": [
    {"path": "services/api/handler.go", "mode": 33188, "blob": "<sha256>"},
    {"path": "services/api/old.go", "mode": 0}
  ]
}
```

`mode: 0` 表示删除。changeset ID 是规范 JSON 编码的 SHA-256。merge 也只列相对第一个父节点的改动，对应文章脚注里的说法：在 monorepo 的主干开发里，大家只关心主线上的历史和 blame。

### 对象（`internal/object/object.go`、`tree.go`）

- **blob**：文件内容
- **tree**：目录快照，格式和 git 类似（`<八进制 mode> <name>\0<32 字节 id>`）。相同的目录在任何 changeset 里哈希都相同，所以客户端从不重复请求已经有的子树
- **unode**：带历史的节点（借用 Meta/Mononoke 的说法）。它记录产生它的 changeset（linknode），以及同一路径上的上一个版本

每个对象的 ID 是 `sha256("<kind> <len>\0" + content)`。**存储形式** `[kind][codec][payload]` 在每一层都一样：服务端磁盘、区域缓存内存、节点 daemon 磁盘，以及网络上传输的都是同一份字节。这对应文章里的说法：cache 直接从内存流式发送 daemon 要的磁盘格式，不需要现算缺什么，也不做 delta 压缩。只有 blob 会压缩，tree 和 unode 主要是哈希，压不下去。

客户端解码 tree 时会拒绝 `..`、`/` 之类的条目名，恶意服务端没法让客户端写到工作区外面。

## 2. 存储（`internal/store`）

文章里有两种存储角色，这里都是接口：

| 角色 | 接口 | 现有实现 | 生产环境可换成 |
|---|---|---|---|
| 权威元数据（changeset 日志、ref、派生数据） | `store.Meta` | SQLite，WAL 模式（`meta.go`）；Postgres（`postgres.go`） | Spanner、CockroachDB |
| 不可变对象 | `store.Objects` | 本地目录 `FS`、内存 LRU `Mem`、两者叠加 `Cached` | S3、GCS |

SQLite 的表结构：

- `changesets(seq, id, data, created)`：追加写的日志，`seq` 就是日志位置，`/api/wal` 可以按位置订阅；`created` 是这一条最后一次被追加的时间，供 gc 的宽限期使用
- `refs(name, target, version)`：**移动 ref 就是对这一行做 compare-and-swap**，和创建 changeset 完全分开
- `derived(kind, key, value)`：派生数据缓存，任何服务端都能计算后写回
- `mapping(ns, key, value)`：git sha 到 changeset（`git`）以及 changeset 到 git sha（`git-export`）的映射，用来做增量导入和导出

创建 changeset 只需要一次插入（如果已经存在，就只刷新 `created`）：不加锁、不需要跨服务器协调、不碰 ref。

对象总是先于引用它们的元数据写入，所以服务端的对象必须扛得住断电。它的对象目录（`store.OpenDurableFS`）按批落盘：`Put` 只记下新写的对象，仓库在追加 changeset 或存派生数据之前调用 `store.Sync`，保证元数据引用的东西都已经在盘上。Linux 上一次 `syncfs(2)` 就能覆盖整批；其他平台逐个 fsync 记下的文件，再 fsync 它们的分片目录。这样每个 changeset 只同步一次，而不是每个对象两次，大规模导入的速度和完全不同步差不多。缓存和工作副本不做这些同步，丢了的对象再取一次就行。所有对象目录都把空文件当作不存在（存储形式的对象至少有两个字节的头部，空文件只可能是崩溃时没落盘的写入留下的），这样对象会被重新获取或上传，而不是以空内容返回。

### 多台服务端（`--meta`、`--objects`）

默认配置下只有一个权威服务端进程。`kudzu server --meta postgres://… --objects s3://bucket/prefix` 把两种存储都放到共享服务里，于是可以在负载均衡后面跑任意多台无状态服务端，正是文章的设计：

- **Postgres** 用同样的四张表。移动 ref 是 `UPDATE refs … WHERE name = $1 AND target = $old` 再检查影响的行数，所以不同服务端上的 land 竞争方式和单进程完全一样：输的一方 rebase 后重试。派生数据用 `INSERT … ON CONFLICT DO NOTHING`，因为每台服务端算出的值都一样。唯一要注意的是日志：序列在插入时而不是提交时分配 `seq`，两个并发追加可能乱序提交，`/api/wal` 的读者就可能漏掉一条。所以追加时会在单行插入外面持有一个事务级 advisory lock，保证提交顺序就是日志顺序。
- **兼容 S3 的存储桶**把对象存在 `prefix/ab/cdef…` 下。对象不可变，服务端之间不需要协调，每台服务端的内存缓存可以无限期保留任何对象。Kudzu 自己签名请求（AWS Signature V4，用 AWS 文档里的示例校验过），不依赖云厂商 SDK。`gs://` 走 GCS 兼容 S3 的 XML API（HMAC 密钥）；`?endpoint=` 可以指向 MinIO、R2 或其他兼容 S3 的服务。gc 的 touch 是把对象在服务端复制到自身，刷新 `Last-Modified` 而不传输数据。

设置 `KUDZU_TEST_POSTGRES` 和 `KUDZU_TEST_S3` 后，repo 和端到端测试也会跑在这些后端上（`internal/store/storetest`）；CI 用 Postgres 和 SeaweedFS 的 S3 网关跑一遍。

### 垃圾回收（`internal/repo/gc.go`）

CAS 竞争失败后重试的 land、遇到冲突的 rebase、从没 land 的提交，都会留下没有任何 ref 能到达的 changeset 和对象。`kudzu gc --store DIR --keep 30d` 负责回收：

1. **标记**：从 ref 能到达的 changeset，以及宽限期（`--keep`）内追加的 changeset，连同它们的祖先，都是活的。活 changeset 的根 tree 及其下所有对象、根 unode 及其子节点和历史版本，都是活的。已经标记过的子树直接跳过，所以标记的开销约等于不同 tree 的数量。
2. **清除**：删除宽限期之前、不可达的 changeset，以及它们的派生数据（`tree`、`gen`、`unode`，还有它们 unode 上的 `blame`）和指向它们的 `mapping` 行。然后删除既没被标记、在宽限期内也没被写入或 touch 过的对象，顺带清掉残留的临时文件。

日志的位置永远不变：被删的条目在 `seq` 里留下空洞，`/api/wal` 的读者自然跳过，`since` 游标依然有效。用墓碑只会让日志无谓地增长。

gc 可以在服务端运行时执行，靠的是宽限期：服务端告诉客户端"这个对象我已经有了"（`/objects/missing`）或新 changeset 复用已有对象时，会 touch 这个对象（刷新 mtime）；changeset 被重新追加、被新 changeset 当作父节点、或者有 ref 移到它上面时，都会刷新 `created`。gc 扫完对象后，会再从 ref 和期间新追加的 changeset 标记一遍；删除 changeset 时先子后父，且只删仍然过期的，被 touch 过的会保留，连同它的祖先和它们需要的对象。对象删除前先移到一边、再检查一次时间。创建 changeset 时，父节点是查日志而不是查缓存，所以不会有东西建在已被回收的 changeset 上。前提是宽限期远大于服务端的 touch 间隔（10 分钟）；`--keep 0s` 只适合没人使用的存储。`--dry-run` 只报告会删除什么。

## 3. 派生数据引擎（`internal/repo/derive.go`）

changeset 只记录"改了什么"，回答不了"这一行最后是谁改的"。文章的做法是：凡是不属于 changeset 本身的东西都算派生数据，并且**统一用一个机制计算**。每种派生数据都递归定义：某个 key 的值，由它各个父节点的值算出来。

```go
type Deriver struct {
    Name    string
    Parents func(ctx, r, key) ([]object.ID, error)
    Compute func(ctx, r, key, parentValues [][]byte) ([]byte, error)
}
```

`Repo.Derive` 负责其余所有事情：查缓存、按需从最老的缺失祖先开始往前算（迭代实现，再长的历史也不会栈溢出）、把结果写回 `derived` 表、合并同一个 key 的并发计算。合并后的计算不受发起它的调用方取消的影响，所以一个调用方中途断开（比如一次很大的 blame 时客户端挂断）不会让等同一个值的其他调用方失败；`Compute` 里的 panic 会变成每个等待者收到的错误，而不是让这个 key 永远卡住。

现有四种：

| 名字 | key | 值 | 用途 | 默认 |
|---|---|---|---|---|
| `tree` | changeset | root tree ID | checkout、diff、读文件 | 创建时同步计算（也起校验作用） |
| `gen` | changeset | 代数 + 第一父节点跳表（`skip[k]` 是往上 2^k 步的祖先） | O(log n) 的祖先判断和 `main~N` | 立即计算 |
| `unode` | changeset | root unode ID | 文件或目录的路径历史 | 立即计算（`--lazy unode` 可改成按需） |
| `blame` | 文件 unode | 每行归属的 changeset | blame | 按需（第一次查询时计算） |

文章说到的三个好处在这里都成立：
1. 加一个新索引只需要再写一个 `Deriver`
2. 每种类型可以选择立即算还是按需算（`Repo.Eager`）
3. 客户端看不到这些结构，内部格式可以随时改

**路径历史为什么快**：每次 changeset 改到某个文件或目录，都会给它生成一个新 unode，并指向同一路径上的上一个 unode。`log <path>` 只需要顺着这条链走，每条结果读一个节点，和中间夹着多少无关历史没有关系。

**blame**：用文件 unode 的上一个版本当父节点。没变的行沿用上一版的归属，新增或修改的行归到当前 linknode。差异计算用 Myers diff（`internal/textdiff`）。

## 4. 服务端写操作（`internal/repo/write.go`）

- **Create**：校验路径和 blob，去掉无效改动，把改动应用到父节点的 tree 上（只重写改动路径上的目录，成本是"改动文件数 × 目录深度"），追加到日志，再计算需要立即计算的派生数据
- **三方合并 `threeWay`**：对每个改动的路径，比较 base 和 ours。上游没动过的直接应用；两边都改了的文本文件做行级三方合并；其余情况算冲突，返回冲突路径
- **Rebase**：在服务端把 `merge-base(head, onto)..head` 这一串 changeset 逐个移到 `onto` 上
- **Merge**：生成一个双父节点的 changeset，改动相对 ours 列出
- **Land**：没有全局锁的乐观循环：

```
loop:
    tip = 读 ref
    如果 head 已经在 tip 里 → 完成
    next = head 是 tip 的后代 ? head : rebase(head, tip)
    CAS(ref, tip, next) 成功 → 完成；失败说明别人先 land 了 → 重来
```

这对应文章里"merge queue 的全局锁在 agent 规模下不适用"的结论。并发的改动只要不冲突，就会一个接一个自动 rebase 进去（测试里 40 个并发 land 全部成功，主线保持线性）。冲突交给调用方（通常是 agent）处理。文章的思路是把测试放到合并之后、部署之前来做，Kudzu 不涉及测试这一步。

## 5. 祖先查询（`internal/repo/ancestry.go`）

- `IsAncestor(a, b)`：先沿 b 的第一父链用跳表做二进制提升，跳到代数不小于 a 的最老节点，看是不是 a。主干开发下 O(log n)。不是的话，再做一次按代数剪枝的完整搜索（处理经由 merge 第二父节点的情况）
- `MergeBase(a, b)`：两边同时着色，按代数从高到低出队，第一个同时带两种颜色的节点就是一个最优公共祖先
- `NthAncestor`：按 n 的二进制位从低到高跳。深度为 D 的节点拥有所有 2^k ≤ D 的跳表层，所以只要 n ≤ D，需要的那一层一定存在

## 6. 对象协议（`internal/proto`）

```
GET  /objects/{id}      单个对象（存储形式），Cache-Control: immutable
POST /objects/fetch     请求体：若干个 32 字节 ID 直接拼接
                        响应：逐个对象的帧流  id(32) | 状态(1) | 长度(4) | 数据
```

**协商只看"想要什么"**：客户端从不告诉服务端自己有什么，只按 ID 要具体的对象。从 root tree 开始，每一层把本地缺的 tree 一次性批量要齐，展开后再要下一层；tree 都拿到之后再批量要 blob。好处：

- 每个请求都是针对一个永不改变的对象，任何缓存都能回答
- 没有一次性的几 GB packfile，按对象流式返回；连接断了只重新要还没收到的那些（`Remote.fetchChunk`）
- sparse checkout 不需要协议特别支持：客户端不要不需要的子树就行

写入方向：`POST /objects/missing` 先问服务端缺哪些 blob，`POST /objects/upload` 只传缺的，服务端逐个校验哈希。

## 7. 缓存层（`internal/tier`）

`kudzu cache`（内存）和 `kudzu daemon`（磁盘，unix socket）是同一份代码，只是本地存储不同：

- 命中直接返回；未命中的批量向上游要
- **合并并发未命中**：同一对象如果已经有拉取在进行，新请求直接等它的结果（测试：50 个并发冷请求 → 上游 1 次请求）。拉取在后台独立 context 里进行，发起它的请求断开也不影响其他等待者
- **限制上游并发数**（`--upstream-concurrency`），实现文章说的故障域隔离：一个区域缓存出问题，不会把上游服务端一起拖垮
- 非对象请求（`/api/*`、上传）原样转发给上游，所以对客户端来说三层没有区别
- daemon 可以用 `--seed` 从快照启动（`kudzu snapshot` 生成，内容是某个版本全部对象的帧流），之后只拉增量。对应文章里"daemon 从定期刷新的快照启动"
- `/stats` 返回命中率、合并数、上游请求数
- 能连上某一层的人就能通过它使用全部 API，所以 unix socket 创建时的权限是 0600（只有创建者能连），要共用 daemon 就显式加 `--socket-mode 0660` 或 `0666`。默认的 socket `/tmp/kudzu.sock` 所在目录人人可写，谁先绑定这个路径，谁就能收到所有任务的请求。因此 `proto.Dial` 连上之后、发送任何内容之前会检查：socket 在所有人可写的目录里，而属主既不是当前用户也不是 root，就拒绝使用（`internal/proto/socket_unix.go`）。多用户共用的 daemon 应该放在不是所有人可写的目录里。daemon 的对象目录默认放在每个用户自己的目录（`<用户缓存目录>/kudzu/daemon`，权限 0700），不再是临时目录里的固定路径

## 8. 客户端（`internal/client`）

- **sparse 规则**（`internal/sparse`，和 git 的 cone mode 一样）：选中的目录下所有内容，加上它各级祖先目录里直接放着的文件
- **clone**：按层批量拉 tree，最后批量拉 blob。blob 从拉取流里出来后，在 worker 池里校验哈希并直接写成工作区文件，不会先落盘再读回来
- **本地缓存**：tree 总是缓存在 `.kudzu/objects`；blob 只在远端不是本机 daemon 时才缓存，因为 daemon 已经替整台机器缓存了（`State.CacheBlobs`）。缓存有容量上限，按最近使用淘汰
- **update**：同时遍历新旧两棵 tree，哈希相同的子树直接跳过，成本取决于差异大小
- **status**：size 和 mtime 跟索引一致就不读文件，否则读出来算哈希
- **所有权**：命令从当前目录往上找 `.kudzu` 目录，只有它属于当前用户时才使用（和 git 的 "dubious ownership" 规则一样），所以放在共享目录里的 `.kudzu` 不能把命令和它发送的 token 引到别的服务端。`KUDZU_SAFE_DIRECTORY` 可以绕过这个检查
- **commit**：先问服务端缺哪些 blob，只上传缺的，再请求服务端创建 changeset。changeset 一创建就在服务端了，没有 push 这一步

### 虚拟化 checkout（`internal/vfs`、`internal/fusefs`）

`kudzu mount [-r REV] [--sparse dirs] REMOTE MOUNTPOINT` 把一个版本挂载成只读文件系统，事先什么都不下载，任务只为它真正读到的内容付费：

- 目录第一次被列出、或者第一次有路径经过它时，才取它的 tree 对象；文件第一次被读时，才取它的 blob。都通过对象协议（通常是节点 daemon）获取，像其他对象一样校验，并放进本地缓存（内存，或 `--cache DIR`）。同一对象的并发读取共用一次请求。
- tree 里不记录文件大小，而 `stat` 在读文件之前就要报告大小。`GET /api/sizes?tree=ID` 一次返回一个目录里所有文件的大小（服务端读取它本来就有的 blob），所以 `ls -l` 的结果是准确的，又不传输任何文件内容。
- 版本不会变，所以内核可以无限期缓存目录项、属性和页面（`FOPEN_KEEP_CACHE`、很长的超时）。文件的 mtime 取 changeset 的时间。sparse 和权限过滤的规则与普通 checkout 相同。
- `internal/vfs` 不依赖 FUSE，可以单独测试；`internal/fusefs` 用 [go-fuse](https://github.com/hanwen/go-fuse) 做适配，只有加 `-tags fuse` 才编译，默认二进制不包含它。可写挂载（`kudzu commit` 读取的 overlay）留待以后。

测试会挂载一个版本，依次运行 `ls`、`ls -l`、`cat`、`grep -r`，每一步之后都检查只取了真正读到的对象。

## 9. git 导入（`internal/gitimport`）

文章说他们的迁移管道是 git 到 Scion 的单向导入，这里也一样：

- `git rev-list --topo-order --reverse --parents` 拿到提交顺序
- 只开一个 `git cat-file --batch` 进程读提交、tree 和 blob
- 自己对比两棵 git tree（哈希相同的子树跳过），得到相对第一父节点的改动
- 新 blob 在 worker 池里并行做哈希、压缩、写入
- `mapping` 表记录 git sha 到 changeset 的对应关系，重复执行只导入新提交，所以可以用来持续镜像一个 git 仓库
- Kudzu 的 ref 只会快进到导入的最新提交（用 compare-and-swap）：ref 还不存在，或者它是新提交的祖先时才移动。`kudzu up` 每次启动都会重新导入，而上次导入之后在 Kudzu 里 land 的 changeset 不在 git 里，所以带着这些 changeset 的 ref 会保持原样，并打印警告。想保留它们，就用 `kudzu export` 导出到一个 git 分支，在 git 里合并后再导入（导出的提交会映射回同一批 changeset，所以合并结果能让 ref 快进）；`--force` 则强制移动 ref
- shallow clone 的边界提交会被当作历史的起点

**导出（`internal/gitexport`）**方向相反：agent 在 Kudzu 里工作的同时，团队现有的 git 工具（代码托管、clone 仓库的 CI、代码搜索）可以继续对着一个镜像运行。`kudzu export --store DIR --git mirror.git --ref main`：

- 从 ref 往回走，遇到 git 已有的 changeset 就停：之前导出过的（`mapping` 的 `git-export` 命名空间，changeset → sha），或者导入时的 `extra.git` 提交仍然映射回它自己的。每个候选都用 `git cat-file --batch-check` 在目标仓库里确认，所以导出到没有这些提交的仓库时会重新写出它们。
- 其余的按父先子后的顺序，交给一个 `git fast-import` 进程。changeset 本来就是相对第一父节点列出改动，正好就是 fast-import 的提交格式（`from`、`merge`、`M`、`D`）；删除放在前面，文件和目录才能互换。相同的 blob 只发送一次。作者转换成 git 身份（author 和 committer），提交说明和时区都保留。
- 用 `git update-ref 新值 旧值` 移动分支，这是一次 compare-and-swap，默认只允许 fast-forward，`--force` 才能覆盖。
- 两个方向的映射都写进 `mapping`，所以下次导出是增量的，之后再从镜像 `kudzu import` 也能认出这些提交，不会重复导入。

## 10. 权限控制（`internal/auth`、`internal/server/views.go`）

文章提到可以给单个目录设权限，这样敏感代码也能放进 monorepo。`kudzu server --auth policy.json` 开启它：

- **认证**：除 `/healthz` 外，每个请求都要带 `Authorization: Bearer <token>`。策略文件里只存 token 的 SHA-256（用 `kudzu token` 生成），每个哈希对应一个主体。没带 token 的请求使用可选的 `anonymous` 主体，否则返回 401。
- **规则**：每个主体有 `read`、`write`、`deny` 路径前缀和可以移动的 `land` ref。最长的匹配前缀决定结果，长度相同时 deny 优先于 write，write 优先于 read。
- **读**：`cat`、`blame`、`ls`、`diff`、带路径的 `log` 都检查读权限，不允许时返回 403 并指明路径。目录列表、diff 和改动列表会去掉调用方不能读的条目；只改了这类路径的 changeset 不会出现在 `log` 里，在 WAL 里只以 `{seq, id, hidden: true}` 出现，读者的游标仍能前进。
- **写**：写权限在 `repo.Create` 里检查（通过 `repo.WithChangeCheck`），所以无论是提交、rebase、merge 还是 land 时的 rebase，新 changeset 列出的每个路径都会被检查；检查发生在去掉无效改动之前，所以不能借写操作验证对不可读文件内容的猜测。移动 ref（land 或 CAS）需要对应的 `land` 规则，并且 `repo.MoveRef` 会检查 ref 新旧 tree 之间内容不同的每个路径：fast-forward 到别人的 changeset、第一父节点无关的 merge、回滚，改变 ref 内容的程度和新 changeset 一样。被拒绝的路径如果调用方连读都不能读，错误信息里不会出现；merge 和 rebase 的冲突也只给出数量，不列出这些路径。
- **对象**：这是最难的部分。对象按内容寻址，任何一层都按 ID 提供，不带路径。Kudzu 的做法是：调用方只能取到它说得出 ID 的对象，所以绝不让它知道不能读的东西的 ID。凡是 API 返回 tree 的地方（resolve、show、ls、create、rebase、merge、snapshot），读权限受限的调用方拿到的都是**过滤后的 tree**：同一个目录去掉不能读的条目，写成普通的 tree 对象。客户端照常校验，所以 clone、update、commit、land 都不用改。完全可读的子树保留原 ID，过滤只发生在规则边界上的目录，结果按主体缓存。提交仍然基于真实的 changeset，所以从过滤后的 checkout 提交并 land，不会丢掉看不到的文件。
- **缓存层**：缓存和 daemon 在 `/api/*` 和上传上转发客户端的 token，向上游取对象时用自己的 `KUDZU_TOKEN`。它们把缓存的对象提供给能连上的任何人，在这个模型下只会暴露调用方本来就说得出 ID 的东西。剩下的泄露是存在性判断：能准确猜出文件内容的人，可以确认仓库里有这个文件。

## 11. 哪些是文章明说的，哪些是 Kudzu 自己的选择

文章明说的：changeset 是 WAL 记录、只存相对第一父节点的改动、内容寻址的 blob；ref 是 Spanner 单行 CAS；派生数据递归定义、统一机制、可选立即或按需；三层缓存响应相同 RPC 且强制分层；按"想要什么"协商、从 root tree 递归；没有 packfile；服务端做 merge 和 rebase；sparse 和虚拟化 checkout；先合并、后测试。

文章没说、Kudzu 自己选的（可能和 Scion 不同）：
- 具体的序列化格式、哈希算法（SHA-256）和压缩方式（zlib）
- 用 unode 实现路径历史（Mononoke 的做法；文章只说有专门的结构）
- 跳表的具体定义（第一父链上的二进制提升）
- 三方合并的规则：相邻的修改也算冲突，比较保守
- 传输层用 HTTP/1.1 加自定义帧格式，没用 gRPC，这样不需要代码生成，用 curl 也能调
- 默认用 SQLite 和本地目录，多台服务端时用 Postgres 和兼容 S3 的存储，而不是 Spanner 和云存储

## API 一览

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/resolve?rev=` | 版本号 → changeset + root tree。版本号可以是 ref、ID 前缀，后面可以加 `~N` / `^N` 后缀（如 `main~2^2`） |
| GET | `/api/changesets/{rev}` | changeset 详情 |
| POST | `/api/changesets` | 创建 changeset；可选 `"land": "main"`，同一次调用里直接 land |
| GET | `/api/log?rev=&path=&limit=` | 第一父链历史，或某个路径的历史 |
| GET | `/api/blame?rev=&path=` | 每行的 changeset、作者、时间 |
| GET | `/api/diff?from=&to=&path=&merge_base=1&patch=1` | 文件列表，可附带 unified diff |
| GET | `/api/cat?rev=&path=` / `/api/ls?rev=&path=` | 读文件 / 列目录 |
| GET | `/api/ancestry?a=&b=` | merge base、谁是谁的祖先 |
| GET / POST | `/api/refs`、`/api/refs/{name}` | 列出 ref / CAS 移动 ref |
| POST | `/api/rebase`、`/api/merge`、`/api/land` | 服务端 rebase / merge / 无锁 land |
| GET | `/api/wal?since=` | 按日志位置订阅新 changeset（例如给代码搜索建索引） |
| GET | `/api/snapshot?rev=&sparse=` | 某个版本的全部对象（daemon 启动用） |
| GET / POST | `/objects/{id}`、`/objects/fetch` | 对象协议，三层都支持 |
| POST | `/objects/missing`、`/objects/upload` | 上传前询问缺哪些对象 / 上传 |
| GET | `/stats`、`/healthz` | 统计 / 健康检查 |
