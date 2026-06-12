# Kaniko Rust 重构详细设计方案

## 1. 项目概述

### 1.1 背景与动机

当前 kaniko 使用 Go 语言实现，编译后的二进制体积约 53MB（经 UPX 压缩后 16MB），存在以下痛点：

| 问题 | 具体表现 |
|------|----------|
| 二进制体积大 | 53MB 原始体积，moby/swarmkit(1.5MB)、aws-sdk-s3(1.5MB)、envoyproxy(2MB+) 为间接依赖无法裁剪 |
| 内存安全 | Go 的 GC 带来运行时开销，且无法提供编译期内存安全保证 |
| 系统调用封装 | `unix.SYS_SYNCFS` 等 Linux 特有系统调用需要 `golang.org/x/sys/unix` 间接封装 |
| 交叉编译 | macOS 无法直接编译（需 GOOS=linux），缺乏真正的跨平台构建能力 |
| 依赖膨胀 | `go-containerregistry` 引入大量传递依赖（150+ 包） |

### 1.2 目标

1. **体积缩减**：目标二进制 < 10MB（未经压缩），通过 Rust 的零成本抽象和静态链接实现
2. **性能提升**：利用 Rust 的零开销抽象和无 GC 运行时，减少构建延迟
3. **内存安全**：编译期保证内存安全，消除运行时数据竞争
4. **API 兼容**：保持与原 kaniko 相同的命令行参数和执行语义
5. **渐进迁移**：支持 Go/Rust 混合运行，逐步替换模块

### 1.3 排除方案

- **Zig**：零容器生态，无 OCI/Dockerfile 相关库，不纳入考虑
- **FFI 调用 Go 库**（路径 B）：CGO 开销大、构建复杂度高、跨语言调试困难
- **完全重写**（路径 A）：风险过高、周期过长

**选择路径 C**：自建 Rust OCI 镜像构建库，结合现有 Rust 生态 crate 进行增量开发。

---

## 2. Rust OCI 生态现状

### 2.1 关键 Crate 调研

| Crate | 版本 | 下载量 | 功能覆盖 | 对标 Go 包 |
|-------|------|--------|----------|-----------|
| `oci-client` | 0.17.0 | 387万 | Registry push/pull, 认证, manifest 操作 | `go-containerregistry/pkg/v1/remote` |
| `oci-distribution` | 0.11.0 | — | OCI Distribution Spec 客户端 | `go-containerregistry/pkg/v1/remote` |
| `dockerfile-parser` | 0.9.0 | — | Dockerfile 解析（仅解析，不执行） | `moby/buildkit/frontend/dockerfile/parser` |
| `tar` | 0.4.x | 3亿+ | tar 归档读写 | `archive/tar` |
| `sha2` | 0.10.x | 2亿+ | SHA-256 哈希 | `crypto/sha256` |
| `reqwest` | 0.12.x | 3亿+ | HTTP 客户端 | `net/http` |
| `nix` | 0.29.x | 1亿+ | Linux 系统调用封装 | `golang.org/x/sys/unix` |
| `tokio` | 1.x | 3亿+ | 异步运行时 | goroutine 调度器 |

### 2.2 需要自建的模块

以下功能在 Rust 生态中无成熟替代，需要自建：

| 模块 | 功能 | 优先级 |
|------|------|--------|
| `oci-image` | OCI Image Spec 数据模型 + 层操作（mutate/append） | P0 - 核心阻塞项 |
| Dockerfile 指令执行器 | COPY/ADD/RUN/ENV 等 18 种指令的执行逻辑 | P0 |
| 文件系统快照 | 增量文件系统 diff + whiteout 文件生成 | P0 |
| Credential Helper | docker-credential-* 子进程调用 | P1 |
| 多阶段构建 | 跨 stage 依赖解析 | P1 |

---

## 3. Crate 架构设计

### 3.1 Workspace 结构

```
kaniko-rs/
├── Cargo.toml                    # workspace 根
├── crates/
│   ├── kaniko-core/              # 核心构建引擎
│   │   ├── Cargo.toml
│   │   └── src/
│   │       ├── lib.rs
│   │       ├── builder.rs        # stageBuilder 对标
│   │       ├── composite_key.rs  # 缓存键计算
│   │       └── stage.rs          # 多阶段构建编排
│   │
│   ├── oci-image/                # OCI Image Spec 实现（核心自建模块）
│   │   ├── Cargo.toml
│   │   └── src/
│   │       ├── lib.rs
│   │       ├── manifest.rs       # OCI Manifest
│   │       ├── config.rs         # Image Config
│   │       ├── layer.rs          # Layer 抽象 + tar 操作
│   │       ├── mutate.rs         # 镜像变更（追加层/修改配置）
│   │       ├── whiteout.rs       # OCI whiteout 规范实现
│   │       ├── digest.rs         # SHA-256 摘要计算
│   │       └── index.rs          # Image Index (manifest list)
│   │
│   ├── oci-registry/             # Registry 交互
│   │   ├── Cargo.toml
│   │   └── src/
│   │       ├── lib.rs
│   │       ├── push.rs           # 镜像推送
│   │       ├── pull.rs           # 镜像拉取
│   │       ├── auth.rs           # 认证（basic/bearer/token）
│   │       ├── credential.rs     # credential helper 集成
│   │       └── transport.rs      # HTTP 传输 + 重试
│   │
│   ├── dockerfile-parser/        # Dockerfile 解析（增强现有 crate）
│   │   ├── Cargo.toml
│   │   └── src/
│   │       ├── lib.rs
│   │       ├── parse.rs          # AST 解析
│   │       ├── instruction.rs    # 指令类型定义
│   │       └── args.rs           # 构建参数解析
│   │
│   ├── kaniko-snapshot/          # 文件系统快照
│   │   ├── Cargo.toml
│   │   └── src/
│   │       ├── lib.rs
│   │       ├── layered_map.rs    # 增量文件状态追踪
│   │       ├── snapshotter.rs    # 快照核心逻辑
│   │       └── walker.rs         # 文件系统遍历 + ignore list
│   │
│   ├── kaniko-cache/             # 层缓存
│   │   ├── Cargo.toml
│   │   └── src/
│   │       ├── lib.rs
│   │       ├── registry.rs       # Registry 缓存
│   │       └── layout.rs         # OCI Layout 本地缓存
│   │
│   ├── kaniko-creds/             # 凭证管理
│   │   ├── Cargo.toml
│   │   └── src/
│   │       ├── lib.rs
│   │       ├── keychain.rs       # 系统凭证链
│   │       └── helper.rs         # credential helper 进程调用
│   │
│   └── kaniko-cli/               # CLI 入口
│       ├── Cargo.toml
│       └── src/
│           ├── main.rs
│           └── args.rs           # 命令行参数定义
│
├── tests/
│   └── integration/              # 集成测试
└── examples/
    └── simple_build.rs
```

### 3.2 依赖关系图

```
kaniko-cli
  ├── kaniko-core
  │     ├── oci-image
  │     ├── oci-registry
  │     ├── dockerfile-parser
  │     ├── kaniko-snapshot
  │     ├── kaniko-cache
  │     └── kaniko-creds
  ├── oci-image
  └── oci-registry

oci-registry
  ├── oci-image
  ├── kaniko-creds
  └── reqwest + tokio

kaniko-snapshot
  ├── oci-image (layer 类型)
  └── nix (Linux syscall)

kaniko-cache
  ├── oci-image
  └── oci-registry
```

---

## 4. 核心模块 API 设计

### 4.1 `oci-image` — OCI Image Spec 实现

这是整个重构的**核心阻塞模块**，对标 Go 的 `go-containerregistry/pkg/v1`。

```rust
// crates/oci-image/src/lib.rs

/// OCI Image Manifest
pub struct Manifest {
    pub schema_version: u32,
    pub media_type: Option<MediaType>,
    pub config: Descriptor,
    pub layers: Vec<Descriptor>,
    pub annotations: Option<BTreeMap<String, String>>,
}

/// Image Config（对标 v1.ConfigFile）
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ImageConfig {
    pub created: Option<String>,
    pub author: Option<String>,
    pub architecture: String,
    pub os: String,
    pub config: ContainerConfig,
    pub rootfs: RootFs,
    pub history: Vec<HistoryEntry>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ContainerConfig {
    pub user: Option<String>,
    pub exposed_ports: Option<BTreeMap<String, ()>>,
    pub env: Option<Vec<String>>,
    pub entrypoint: Option<Vec<String>>,
    pub cmd: Option<Vec<String>>,
    pub volumes: Option<BTreeMap<String, ()>>,
    pub working_dir: Option<String>,
    pub labels: Option<BTreeMap<String, String>>,
    pub stop_signal: Option<String>,
    pub shell: Option<Vec<String>>,
}

/// Layer 抽象
pub struct Layer {
    media_type: MediaType,
    digest: Sha256Digest,
    size: u64,
    diff_id: Sha256Digest,
    annotations: Option<BTreeMap<String, String>>,
    data: Vec<u8>,  // tar 内容
}

/// Image 抽象（对标 v1.Image）
pub trait Image: Send + Sync {
    fn manifest(&self) -> Result<&Manifest>;
    fn config_file(&self) -> Result<ImageConfig>;
    fn layers(&self) -> Result<Vec<Box<dyn LayerReader>>>;
    fn digest(&self) -> Result<Sha256Digest>;
    fn size(&self) -> Result<u64>;
}

/// 镜像变更操作（对标 v1/mutate）
pub mod mutate {
    /// 追加层到镜像
    pub fn append_layers(
        image: Box<dyn Image>,
        layers: Vec<Layer>,
    ) -> Result<Box<dyn Image>>;

    /// 更新镜像配置
    pub fn update_config(
        image: Box<dyn Image>,
        f: impl FnOnce(&mut ContainerConfig),
    ) -> Result<Box<dyn Image>>;

    /// 设置入口点
    pub fn set_entrypoint(
        image: Box<dyn Image>,
        entrypoint: Vec<String>,
    ) -> Result<Box<dyn Image>>;
}
```

### 4.2 `DockerCommand` Trait — 指令执行器

对标 Go 的 [`commands.DockerCommand`](pkg/commands/commands.go:30) 接口：

```rust
// crates/kaniko-core/src/command.rs

/// Dockerfile 指令执行 trait
/// 对标 Go: commands.DockerCommand
#[async_trait]
pub trait DockerCommand: Send + Sync + fmt::Debug {
    /// 执行指令：修改文件系统 + 更新镜像配置
    /// 对标 Go: ExecuteCommand(*v1.Config, *dockerfile.BuildArgs) error
    async fn execute(
        &self,
        config: &mut ContainerConfig,
        args: &BuildArgs,
    ) -> Result<()>;

    /// 指令字符串表示
    /// 对标 Go: String() string
    fn to_string(&self) -> String;

    /// 返回需要快照的文件列表
    /// 对标 Go: FilesToSnapshot() []string
    fn files_to_snapshot(&self) -> Option<Vec<PathBuf>>;

    /// 是否能提供文件列表（元数据指令返回 true）
    /// 对标 Go: ProvidesFilesToSnapshot() bool
    fn provides_files_to_snapshot(&self) -> bool;

    /// 返回缓存版本的指令
    /// 对标 Go: CacheCommand(v1.Image) DockerCommand
    fn cache_command(&self, cached_image: Box<dyn Image>) -> Option<Box<dyn DockerCommand>>;

    /// 返回依赖的构建上下文文件
    /// 对标 Go: FilesUsedFromContext(*v1.Config, *dockerfile.BuildArgs) ([]string, error)
    fn files_used_from_context(
        &self,
        config: &ContainerConfig,
        args: &BuildArgs,
    ) -> Result<Vec<PathBuf>>;

    /// 是否仅修改元数据
    /// 对标 Go: MetadataOnly() bool
    fn metadata_only(&self) -> bool;

    /// 是否需要解压文件系统
    /// 对标 Go: RequiresUnpackedFS() bool
    fn requires_unpacked_fs(&self) -> bool;

    /// 是否缓存输出层
    /// 对标 Go: ShouldCacheOutput() bool
    fn should_cache_output(&self) -> bool;

    /// 是否可能删除文件
    /// 对标 Go: ShouldDetectDeletedFiles() bool
    fn should_detect_deleted_files(&self) -> bool;

    /// 缓存键是否需要 ARG/ENV
    /// 对标 Go: IsArgsEnvsRequiredInCache() bool
    fn is_args_envs_required_in_cache(&self) -> bool;
}

/// 缓存指令标记 trait
/// 对标 Go: commands.Cached interface
pub trait Cached: DockerCommand {
    fn layer(&self) -> Result<Layer>;
}
```

### 4.3 具体指令实现示例

```rust
// crates/kaniko-core/src/command/copy.rs

/// COPY 指令实现
/// 对标 Go: commands.CopyCommand
pub struct CopyCommand {
    cmd: CopyInstruction,
    file_context: FileContext,
    should_cache: bool,
    files_to_snapshot: RefCell<Vec<PathBuf>>,
}

#[async_trait]
impl DockerCommand for CopyCommand {
    async fn execute(
        &self,
        config: &mut ContainerConfig,
        args: &BuildArgs,
    ) -> Result<()> {
        let srcs = self.resolve_sources(args)?;
        let dest = self.resolve_destination(config, args)?;

        for src in &srcs {
            let entries = self.file_context.resolve(src)?;
            for entry in entries {
                let dest_path = compute_dest_path(&entry, &dest);
                copy_filesystem_entry(&entry, &dest_path)?;
                self.files_to_snapshot.borrow_mut().push(dest_path);
            }
        }

        Ok(())
    }

    fn files_to_snapshot(&self) -> Option<Vec<PathBuf>> {
        Some(self.files_to_snapshot.borrow().clone())
    }

    fn provides_files_to_snapshot(&self) -> bool { true }
    fn metadata_only(&self) -> bool { false }
    fn requires_unpacked_fs(&self) -> bool { false }
    fn should_cache_output(&self) -> bool { self.should_cache }
    fn should_detect_deleted_files(&self) -> bool { false }
    fn is_args_envs_required_in_cache(&self) -> bool { false }
}

// crates/kaniko-core/src/command/run.rs

/// RUN 指令实现
/// 对标 Go: commands.RunCommand
pub struct RunCommand {
    cmd: RunInstruction,
    should_cache: bool,
}

#[async_trait]
impl DockerCommand for RunCommand {
    async fn execute(
        &self,
        config: &mut ContainerConfig,
        _args: &BuildArgs,
    ) -> Result<()> {
        let shell = config.shell.clone()
            .unwrap_or_else(|| vec!["/bin/sh".into(), "-c".into()]);
        let cmd_str = self.cmd.shell_form(&shell);
        let status = Command::new(&shell[0])
            .args(&shell[1..])
            .arg(&cmd_str)
            .status()
            .await?;
        if !status.success() {
            return Err(Error::CommandFailed(cmd_str, status.code()));
        }
        Ok(())
    }

    fn provides_files_to_snapshot(&self) -> bool { false }
    fn metadata_only(&self) -> bool { false }
    fn requires_unpacked_fs(&self) -> bool { true }
    fn should_cache_output(&self) -> bool { self.should_cache }
    fn should_detect_deleted_files(&self) -> bool { true }
    fn is_args_envs_required_in_cache(&self) -> bool { true }
}
```

### 4.4 文件系统快照模块

对标 Go 的 [`snapshot.Snapshotter`](pkg/snapshot/snapshot.go:41)：

```rust
// crates/kaniko-snapshot/src/snapshotter.rs

/// 文件系统快照器
/// 对标 Go: snapshot.Snapshotter
pub struct Snapshotter {
    layered_map: LayeredMap,
    directory: PathBuf,
    ignore_list: Vec<IgnoreEntry>,
}

impl Snapshotter {
    pub fn new(layered_map: LayeredMap, directory: PathBuf) -> Self { ... }

    /// 初始化：扫描完整文件系统
    /// 对标 Go: (s *Snapshotter) Init() error
    pub fn init(&mut self) -> Result<()> {
        let (files, _) = self.scan_full_filesystem()?;
        self.layered_map.set_current_paths(files);
        Ok(())
    }

    /// 生成当前文件系统状态的哈希键
    /// 对标 Go: (s *Snapshotter) Key() (string, error)
    pub fn key(&self) -> Result<String> {
        self.layered_map.key()
    }

    /// 对指定文件列表进行快照，生成 tar 层
    /// 对标 Go: (s *Snapshotter) TakeSnapshot(files []string, shdCheckDelete bool, ...) (string, error)
    pub fn take_snapshot(
        &mut self,
        files: &[PathBuf],
        should_check_delete: bool,
        force_build_metadata: bool,
    ) -> Result<PathBuf> {
        self.layered_map.snapshot();

        if files.is_empty() && !force_build_metadata {
            return Ok(PathBuf::new());
        }

        let resolved = resolve_paths(files, &self.ignore_list)?;
        self.layered_map.add_files(&resolved)?;

        let whiteouts = if should_check_delete {
            self.detect_deleted_files(&resolved)?
        } else {
            vec![]
        };

        let tar_path = self.write_tar(&resolved, &whiteouts)?;
        Ok(tar_path)
    }

    /// 对完整文件系统进行快照
    /// 对标 Go: (s *Snapshotter) TakeSnapshotFS() (string, error)
    pub fn take_snapshot_fs(&mut self) -> Result<PathBuf> {
        let (added, deleted) = self.scan_full_filesystem()?;
        let whiteouts = remove_obsolete_whiteouts(&deleted)?;
        self.write_tar(&added, &whiteouts)
    }
}
```

### 4.5 构建引擎

对标 Go 的 [`stageBuilder`](pkg/executor/build.go:70)：

```rust
// crates/kaniko-core/src/builder.rs

/// 阶段构建器
/// 对标 Go: stageBuilder
pub struct StageBuilder {
    stage: KanikoStage,
    image: Box<dyn Image>,
    config: ImageConfig,
    base_image_digest: String,
    final_cache_key: String,
    opts: KanikoOptions,
    file_context: FileContext,
    commands: Vec<Box<dyn DockerCommand>>,
    args: BuildArgs,
    cross_stage_deps: HashMap<usize, Vec<String>>,
    digest_to_cache_key: HashMap<String, String>,
    stage_idx_to_digest: HashMap<String, String>,
    snapshotter: Snapshotter,
    layer_cache: Box<dyn LayerCache>,
    push_layer_to_cache: CachePusher,
}

impl StageBuilder {
    /// 构建一个阶段
    /// 对标 Go: (s *stageBuilder) build() error
    pub async fn build(&mut self) -> Result<()> {
        let mut composite_key = CompositeCache::new(&self.base_image_digest);

        // 优化：尝试命中缓存
        self.optimize(&mut composite_key).await?;

        // 按需解压文件系统
        if self.should_unpack_fs() {
            self.unpack_filesystem().await?;
        }

        let mut init_snapshot_taken = false;
        let mut cache_handles = vec![];

        for (index, command) in self.commands.iter().enumerate() {
            // 获取上下文文件
            let files = command.files_used_from_context(&self.config.config, &self.args)?;

            // 更新缓存键
            if self.opts.cache {
                composite_key = self.populate_composite_key(
                    command.as_ref(), &files, composite_key, &self.args, &self.config.config.env,
                )?;
            }

            // 初始快照
            if !init_snapshot_taken
                && !self.is_cache_command(command.as_ref())
                && !command.provides_files_to_snapshot()
            {
                self.snapshotter.init()?;
                init_snapshot_taken = true;
            }

            // 执行指令
            command.execute(&mut self.config.config, &self.args).await?;
            let files = command.files_to_snapshot();

            // 判断是否需要快照
            if !self.should_take_snapshot(index, command.metadata_only())
                && !self.opts.force_build_metadata
            {
                continue;
            }

            if self.is_cache_command(command.as_ref()) {
                // 缓存命中：直接使用缓存层
                let cached = command.as_any().downcast_ref::<dyn Cached>().unwrap();
                let layer = cached.layer()?;
                self.save_layer_to_image(&layer, &command.to_string())?;
            } else {
                // 正常路径：快照 → 层 → 镜像
                let tar_path = self.take_snapshot(files, command.should_detect_deleted_files())?;

                if self.opts.cache && command.should_cache_output() && !self.opts.no_push_cache {
                    let ck = composite_key.hash()?;
                    let handle = self.push_layer_to_cache_async(ck, &tar_path, &command.to_string());
                    cache_handles.push(handle);
                }

                self.save_snapshot_to_image(&command.to_string(), &tar_path)?;
            }
        }

        // 等待所有缓存推送完成
        for handle in cache_handles {
            if let Err(e) = handle.await {
                warn!("Error uploading layer to cache: {}", e);
            }
        }

        Ok(())
    }
}
```

### 4.6 Registry 交互

对标 Go 的 [`executor.push.DoPush`](pkg/executor/push.go:174)：

```rust
// crates/oci-registry/src/push.rs

/// 镜像推送
/// 对标 Go: executor.DoPush
pub async fn push_image(
    image: &dyn Image,
    destinations: &[String],
    opts: &PushOptions,
) -> Result<()> {
    for dest in destinations {
        let reference = Reference::parse(dest)?;
        let client = RegistryClient::new(reference.registry().to_string(), opts.auth.clone())?;

        // 1. 检查层是否存在（mount 优化）
        let manifest = image.manifest()?;
        for layer_desc in &manifest.layers {
            if client.blob_exists(&layer_desc.digest).await? {
                continue;
            }
            // 2. 上传层（chunked upload）
            let layer = image.layer_by_digest(&layer_desc.digest)?;
            client.upload_blob_chunked(layer.data(), &layer_desc.digest).await?;
        }

        // 3. 上传 config
        let config_data = serde_json::to_vec(&image.config_file()?)?;
        client.upload_blob(&config_data, &manifest.config.digest).await?;

        // 4. 推送 manifest
        let manifest_data = serde_json::to_vec(manifest)?;
        client.push_manifest(&reference, &manifest_data, &manifest.media_type).await?;

        info!("Pushed {}", dest);
    }
    Ok(())
}
```

---

## 5. 数据流

### 5.1 构建主流程

```
┌──────────────────────────────────────────────────────────────────────┐
│                         kaniko-cli main()                            │
│  解析 CLI 参数 → KanikoOptions                                        │
└────────────────────────────┬─────────────────────────────────────────┘
                             │
                             ▼
┌──────────────────────────────────────────────────────────────────────┐
│                    DoBuild(opts: KanikoOptions)                      │
│  1. 解析 Dockerfile → Vec<KanikoStage>                               │
│  2. 解析 .dockerignore                                                │
│  3. 获取构建上下文                                                     │
│  4. 检查推送权限                                                       │
└────────────────────────────┬─────────────────────────────────────────┘
                             │
                    ┌────────┴────────┐
                    │   遍历 stages    │
                    └────────┬────────┘
                             │
                             ▼
┌──────────────────────────────────────────────────────────────────────┐
│                    StageBuilder::build()                             │
│                                                                      │
│  ┌─────────────┐    ┌──────────────┐    ┌─────────────────────────┐ │
│  │  拉取基础镜像  │───▶│  解压文件系统   │───▶│  初始化 Snapshotter     │ │
│  └─────────────┘    └──────────────┘    └─────────────────────────┘ │
│                             │                                        │
│                             ▼                                        │
│  ┌─────────────────────────────────────────────────────────────────┐│
│  │                    指令执行循环                                    ││
│  │  for command in commands:                                       ││
│  │    1. populate_composite_key → cache key                        ││
│  │    2. command.execute(config, args)  → 文件系统变更 + 配置更新      ││
│  │    3. snapshotter.take_snapshot(files)  → tar 层                 ││
│  │    4. save_snapshot_to_image(tar)  → 追加层到镜像                  ││
│  │    5. push_layer_to_cache (async)                               ││
│  └─────────────────────────────────────────────────────────────────┘│
│                             │                                        │
│                             ▼                                        │
│                    最终镜像 (v1.Image)                                 │
└────────────────────────────┬─────────────────────────────────────────┘
                             │
                             ▼
┌──────────────────────────────────────────────────────────────────────┐
│                      DoPush(image, opts)                             │
│  1. 写 tar 文件（如果 --tar-path）                                     │
│  2. 写 OCI Layout（如果 --oci-layout-path）                            │
│  3. 写 digest 文件                                                     │
│  4. 推送到 Registry（除非 --no-push）                                   │
└──────────────────────────────────────────────────────────────────────┘
```

### 5.2 缓存数据流

```
                     ┌─────────────┐
                     │  缓存查询     │
                     │  composite   │
                     │  key hash    │
                     └──────┬──────┘
                            │
                    ┌───────┴───────┐
                    │               │
              ┌─────▼─────┐   ┌────▼─────┐
              │ 命中缓存    │   │ 未命中     │
              │ CacheCommand│  │ 正常执行   │
              │ 直接用缓存层 │  │ 快照→层    │
              └───────────┘   │ →推送到缓存 │
                              └──────────┘
```

---

## 6. 依赖映射表

### 6.1 Go → Rust 模块映射

| Go 包 | 功能 | Rust 对标 | 实现策略 |
|-------|------|-----------|---------|
| `go-containerregistry/pkg/v1` | OCI Image 数据模型 | `oci-image` | 自建 |
| `go-containerregistry/pkg/v1/mutate` | 镜像变更 | `oci-image::mutate` | 自建 |
| `go-containerregistry/pkg/v1/remote` | Registry 交互 | `oci-registry` | 基于 `oci-client` 封装 |
| `go-containerregistry/pkg/v1/tarball` | tar 镜像读写 | `oci-image::layer` | 基于 `tar` crate |
| `go-containerregistry/pkg/v1/layout` | OCI Layout 读写 | `oci-image::layout` | 自建 |
| `go-containerregistry/pkg/v1/empty` | 空镜像 | `oci-image::empty` | 自建 |
| `go-containerregistry/pkg/v1/partial` | 部分镜像 | `oci-image::partial` | 自建 |
| `go-containerregistry/pkg/name` | 镜像引用解析 | `oci-client::Reference` | 直接使用 |
| `moby/buildkit/.../dockerfile` | Dockerfile 解析 | `dockerfile-parser` + 增强 | 基于现有 crate |
| `moby/buildkit/.../instructions` | 指令类型 | `kaniko-core::instruction` | 自建 |
| `golang.org/x/sys/unix` | Linux syscall | `nix` | 直接使用 |
| `net/http` | HTTP 客户端 | `reqwest` + `tokio` | 直接使用 |
| `archive/tar` | tar 归档 | `tar` | 直接使用 |
| `crypto/sha256` | 哈希 | `sha2` | 直接使用 |
| `os/exec` | 进程执行 | `tokio::process::Command` | 直接使用 |
| `path/filepath` | 路径操作 | `std::path` + `pathdiff` | 标准库 |
| `github.com/spf13/cobra` | CLI 框架 | `clap` | 直接使用 |
| `github.com/sirupsen/logrus` | 日志 | `tracing` | 直接使用 |

### 6.2 18 种 Dockerfile 指令映射

| Go 实现 | 指令 | Rust 结构体 | 复杂度 |
|---------|------|-------------|--------|
| `RunCommand` | RUN | `RunCommand` | 高（进程管理 + 缓存） |
| `CopyCommand` | COPY | `CopyCommand` | 高（文件系统操作 + 缓存） |
| `AddCommand` | ADD | `AddCommand` | 高（URL + tar 解压） |
| `EnvCommand` | ENV | `EnvCommand` | 低 |
| `ExposeCommand` | EXPOSE | `ExposeCommand` | 低 |
| `WorkdirCommand` | WORKDIR | `WorkdirCommand` | 中（需创建目录） |
| `CmdCommand` | CMD | `CmdCommand` | 低 |
| `EntrypointCommand` | ENTRYPOINT | `EntrypointCommand` | 低 |
| `LabelCommand` | LABEL | `LabelCommand` | 低 |
| `UserCommand` | USER | `UserCommand` | 低 |
| `VolumeCommand` | VOLUME | `VolumeCommand` | 中（需创建挂载点） |
| `ArgCommand` | ARG | `ArgCommand` | 低 |
| `StopSignalCommand` | STOPSIGNAL | `StopSignalCommand` | 低 |
| `HealthcheckCommand` | HEALTHCHECK | `HealthcheckCommand` | 中 |
| `ShellCommand` | SHELL | `ShellCommand` | 低 |
| `OnbuildCommand` | ONBUILD | `OnbuildCommand` | 中 |
| `MaintainerCommand` | MAINTAINER | `MaintainerCommand` | 低（已弃用） |
| `CopyCommand`(cache) | COPY --from | `CopyFromCommand` | 高（跨 stage） |

---

## 7. 工作量估算

### 7.1 各模块开发工时（人天）

| 模块 | 设计 | 编码 | 测试 | 合计 | 依赖 |
|------|------|------|------|------|------|
| `oci-image` (核心) | 5 | 20 | 10 | **35** | 无 |
| `oci-registry` | 3 | 12 | 8 | **23** | oci-image |
| `dockerfile-parser` (增强) | 2 | 8 | 5 | **15** | 无 |
| `kaniko-snapshot` | 3 | 10 | 7 | **20** | oci-image |
| `kaniko-creds` | 2 | 5 | 5 | **12** | 无 |
| `kaniko-cache` | 2 | 6 | 4 | **12** | oci-image, oci-registry |
| `kaniko-core` (指令+构建引擎) | 5 | 25 | 15 | **45** | 所有模块 |
| `kaniko-cli` | 2 | 5 | 3 | **10** | kaniko-core |
| 集成测试 | — | — | 15 | **15** | 全部 |
| **总计** | **24** | **91** | **72** | **187** | — |

### 7.2 人力配置建议

| 角色 | 人数 | 职责 |
|------|------|------|
| Rust 资深工程师 | 2 | oci-image 核心 + kaniko-core 构建引擎 |
| Rust 中级工程师 | 2 | 指令实现 + 快照模块 + 测试 |
| DevOps | 1 | CI/CD + 容器构建 + 发布流水线 |

---

## 8. 迁移阶段计划

### 阶段 1：MVP 验证（8 周）

**目标**：能解析简单 Dockerfile 并构建/推送基础镜像

```
Week 1-2: oci-image 核心数据结构
  - Manifest, Config, Layer, Descriptor
  - SHA-256 摘要计算
  - tar 层读写

Week 3-4: dockerfile-parser + 指令框架
  - 基于 dockerfile-parser crate 增强
  - DockerCommand trait 定义
  - 实现 5 个低复杂度指令：ENV, LABEL, EXPOSE, USER, WORKDIR

Week 5-6: 构建引擎核心
  - StageBuilder 骨架
  - 文件系统快照基础
  - 实现 COPY + RUN 指令

Week 7-8: Registry 交互 + 端到端验证
  - oci-registry 基础推送
  - 验证：从 Dockerfile → 镜像推送 → docker pull 运行
```

**MVP 验收标准**：
- 能构建含 FROM + COPY + RUN + ENV 的简单 Dockerfile
- 能推送到 Docker Hub / GCR
- `docker run` 验证镜像功能正确

### 阶段 2：功能对齐（6 周）

**目标**：与 Go 版本功能 1:1 对齐

```
Week 9-10: 剩余指令实现
  - ADD（URL 下载 + tar 解压）
  - VOLUME, HEALTHCHECK, SHELL, ONBUILD
  - ARG 构建参数

Week 11-12: 缓存系统
  - Registry 缓存 + OCI Layout 缓存
  - CompositeCache 键计算
  - CacheCommand 实现

Week 13-14: 多阶段构建 + 认证
  - 多阶段构建编排
  - COPY --from 跨 stage
  - credential helper 集成
  - .dockerignore 支持
```

**功能对齐验收标准**：
- 通过 Go 版本 > 80% 的集成测试用例
- 支持 18 种 Dockerfile 指令
- 支持多阶段构建

### 阶段 3：生产化（4 周）

**目标**：性能优化、安全加固、发布准备

```
Week 15-16: 性能优化
  - 并行层推送（tokio 并发）
  - 流式层上传（避免全量内存缓存）
  - 增量快照优化

Week 17-18: 生产化
  - 容器镜像发布（distroless + musl）
  - 安全审计（cargo audit）
  - 文档 + 迁移指南
  - 性能基准测试 vs Go 版本
```

---

## 9. 风险与缓解

| 风险 | 影响 | 概率 | 缓解措施 |
|------|------|------|---------|
| `oci-image` 模块开发周期超预期 | 阻塞所有下游模块 | 高 | 优先实现最小子集；考虑临时 fork `oci-client` 扩展 |
| Dockerfile 语义差异 | 构建结果与 Go 版本不一致 | 中 | 建立兼容性测试套件，逐指令对比 |
| Registry 兼容性 | 部分私有 Registry 不兼容 | 中 | 对标 `go-containerregistry` 的 transport 实现，覆盖 AWS ECR / GCR / ACR |
| 性能回退 | Rust 版本比 Go 版本慢 | 低 | tokio 异步 I/O + 零拷贝层上传；基准测试驱动优化 |
| Rust 人才短缺 | 开发进度缓慢 | 中 | 阶段 1 用最少人力验证可行性，再决定是否扩大投入 |
| Linux 特有系统调用 | snapshot 模块依赖 `syncfs(2)` 等 | 低 | `nix` crate 已有完善封装；CI 仅在 Linux 上运行 |

---

## 10. 测试策略

### 10.1 测试层次

```
┌─────────────────────────────────────────────┐
│             集成测试（阶段 3）                  │
│  - 完整 Dockerfile 构建                       │
│  - 多 Registry 推送验证                       │
│  - 兼容性测试（vs Go 版本输出）                 │
├─────────────────────────────────────────────┤
│            模块间测试（阶段 2）                  │
│  - Dockerfile → 构建 → 镜像 → 推送 端到端      │
│  - 缓存命中/未命中场景                         │
│  - 多阶段构建                                 │
├─────────────────────────────────────────────┤
│           单元测试（阶段 1 起）                  │
│  - oci-image: Manifest/Config/Layer 序列化    │
│  - dockerfile-parser: 各种 Dockerfile 语法    │
│  - 每个 DockerCommand 实现的 execute()        │
│  - snapshotter: 文件 diff + whiteout          │
│  - 缓存键: CompositeCache.Hash()             │
└─────────────────────────────────────────────┘
```

### 10.2 兼容性测试

```rust
// tests/compat/mod.rs

/// 对比 Rust 版本和 Go 版本的构建输出
async fn compare_build_output(dockerfile: &str) -> Result<()> {
    let rust_image = build_with_rust(dockerfile).await?;
    let go_image = build_with_go(dockerfile).await?;

    // 1. 对比 config（排除 Created 时间戳）
    assert_config_equivalent(&rust_image.config, &go_image.config);

    // 2. 对比层数
    assert_eq!(rust_image.layers.len(), go_image.layers.len());

    // 3. 对比每层的 diff_id
    for (r, g) in rust_image.layers.iter().zip(go_image.layers.iter()) {
        assert_eq!(r.diff_id, g.diff_id);
    }

    Ok(())
}
```

---

## 11. 性能预期

### 11.1 二进制体积对比

| 指标 | Go 版本 | Rust 版本（预期） | 改善 |
|------|---------|------------------|------|
| 原始二进制 | 53 MB | 8-12 MB | -77% ~ -85% |
| UPX 压缩后 | 16 MB | 3-5 MB | -69% ~ -81% |
| 容器镜像（含 base） | ~30 MB | ~15 MB | -50% |
| 运行时内存 | ~50 MB（含 GC） | ~10-15 MB | -70% ~ -80% |

### 11.2 运行时性能预期

| 操作 | Go 版本 | Rust 版本（预期） | 原因 |
|------|---------|------------------|------|
| Dockerfile 解析 | ~5ms | ~3ms | 零拷贝解析 |
| 文件系统快照 | ~200ms | ~150ms | 减少 GC 暂停 |
| 层 tar 打包 | ~500ms | ~400ms | 流式写入 + 零拷贝 |
| Registry 推送 | ~2s/层 | ~1.5s/层 | tokio 异步 + 连接池 |
| RUN 命令执行 | 相同 | 相同 | 受限于子进程 |

---

## 12. `oci-image` 模块详细设计

### 12.1 核心数据流

```
tar 文件 → Layer::from_tar() → Layer { digest, diff_id, data }
                                              │
                                              ▼
                                    mutate::append_layers()
                                              │
                                              ▼
Image { manifest, config, layers } ──▶ serde_json::to_vec() ──▶ Registry Push
```

### 12.2 Layer 创建流程

```rust
// crates/oci-image/src/layer.rs

impl Layer {
    /// 从 tar 数据创建层
    pub fn from_tar(data: Vec<u8>, media_type: MediaType) -> Result<Self> {
        // 1. 计算 digest (SHA-256)
        let digest = Sha256Digest::from_bytes(&data);

        // 2. 计算 diff_id (解压后的 SHA-256，对于未压缩层与 digest 相同)
        let diff_id = if media_type.is_compressed() {
            let mut decoder = GzipDecoder::new(&data[..]);
            let mut uncompressed = Vec::new();
            decoder.read_to_end(&mut uncompressed)?;
            Sha256Digest::from_bytes(&uncompressed)
        } else {
            digest.clone()
        };

        Ok(Layer {
            media_type,
            digest,
            size: data.len() as u64,
            diff_id,
            annotations: None,
            data,
        })
    }

    /// 从文件列表创建 tar 层（快照场景）
    pub fn from_files(
        files: &[PathBuf],
        whiteouts: &[PathBuf],
        root: &Path,
    ) -> Result<Self> {
        let mut tar_data = Vec::new();
        {
            let mut builder = tar::Builder::new(&mut tar_data);

            // 添加文件
            for file in files {
                let rel_path = file.strip_prefix(root)?;
                let mut header = tar::Header::new_gnu();
                let metadata = std::fs::metadata(file)?;
                header.set_path(rel_path)?;
                header.set_size(metadata.len());
                header.set_mode(metadata.permissions().mode());
                header.set_mtime(
                    metadata.modified()?.duration_since(UNIX_EPOCH)?.as_secs(),
                );
                header.set_cksum();

                if metadata.is_file() {
                    let f = File::open(file)?;
                    builder.append(&header, f)?;
                } else if metadata.is_dir() {
                    header.set_entry_type(tar::EntryType::Directory);
                    builder.append(&header, std::io::empty())?;
                }
            }

            // 添加 whiteout 文件
            for whiteout in whiteouts {
                let rel_path = whiteout.strip_prefix(root)?;
                let wh_path = rel_path.parent()
                    .unwrap_or(Path::new(""))
                    .join(format!(".wh.{}", rel_path.file_name().unwrap().to_string_lossy()));
                let mut header = tar::Header::new_gnu();
                header.set_path(&wh_path)?;
                header.set_size(0);
                header.set_entry_type(tar::EntryType::Regular);
                header.set_cksum();
                builder.append(&header, std::io::empty())?;
            }

            builder.finish()?;
        }

        Self::from_tar(tar_data, MediaType::LayerGzip)
    }
}
```

### 12.3 `mutate` 模块

```rust
// crates/oci-image/src/mutate.rs

/// 追加层到镜像
pub fn append_layers(
    image: Box<dyn Image>,
    new_layers: Vec<Layer>,
) -> Result<MutableImage> {
    let mut config = image.config_file()?;
    let mut manifest = image.manifest()?.clone();
    let mut existing_layers = image.layers()?;

    for layer in new_layers {
        // 更新 manifest.layers
        manifest.layers.push(Descriptor {
            media_type: layer.media_type.clone(),
            digest: layer.digest.clone(),
            size: layer.size,
            annotations: layer.annotations.clone(),
        });

        // 更新 config.rootfs.diff_ids
        config.rootfs.diff_ids.push(layer.diff_id.to_string());

        // 更新 config.history
        config.history.push(HistoryEntry {
            created: Some(Utc::now().to_rfc3339()),
            created_by: Some("kaniko".to_string()),
            empty_layer: Some(false),
            ..Default::default()
        });

        existing_layers.push(layer);
    }

    // 重新计算 config digest
    let config_bytes = serde_json::to_vec(&config)?;
    let config_digest = Sha256Digest::from_bytes(&config_bytes);

    // 更新 manifest.config
    manifest.config = Descriptor {
        media_type: MediaType::ImageConfig,
        digest: config_digest,
        size: config_bytes.len() as u64,
        ..Default::default()
    };

    Ok(MutableImage {
        manifest,
        config,
        config_bytes,
        layers: existing_layers,
    })
}
```

---

## 13. Credential Helper 实现方案

### 13.1 设计

对标 Go 的 [`creds.GetKeychain()`](pkg/creds/)：

```rust
// crates/kaniko-creds/src/keychain.rs

/// 凭证提供者 trait
pub trait CredentialProvider: Send + Sync {
    fn credentials(&self, registry: &str) -> Result<Credential>;
}

/// 凭证
pub struct Credential {
    pub username: String,
    pub password: String,
    pub identity_token: Option<String>,
    pub registry_token: Option<String>,
}

/// 系统凭证链（对标 Go 的 keychain）
pub struct SystemKeychain {
    docker_config_path: PathBuf,
}

impl CredentialProvider for SystemKeychain {
    fn credentials(&self, registry: &str) -> Result<Credential> {
        // 1. 读取 ~/.docker/config.json
        if let Ok(config) = self.read_docker_config() {
            if let Some(auth) = config.auths.get(registry) {
                return Ok(auth.decode());
            }
            // 2. 调用 credential helper
            if let Some(helper) = config.cred_helpers.get(registry) {
                return self.call_credential_helper(helper, registry);
            }
            // 3. 尝试默认 credential helper
            if let Some(helper) = &config.creds_store {
                return self.call_credential_helper(helper, registry);
            }
        }
        // 4. 匿名访问
        Ok(Credential::anonymous())
    }
}

impl SystemKeychain {
    /// 调用 docker-credential-* 子进程
    fn call_credential_helper(
        &self,
        helper: &str,
        registry: &str,
    ) -> Result<Credential> {
        let binary = format!("docker-credential-{}", helper);
        let output = Command::new(&binary)
            .arg("get")
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .spawn()?
            .wait_with_output()?;

        // 解析 JSON 输出: {"Username":"...","Secret":"..."}
        let resp: CredentialResponse = serde_json::from_slice(&output.stdout)?;
        Ok(Credential {
            username: resp.username,
            password: resp.secret,
            identity_token: None,
            registry_token: None,
        })
    }
}
```

---

## 14. 技术选型与 Cargo 依赖

### 14.1 核心依赖

```toml
# crates/oci-image/Cargo.toml
[dependencies]
serde = { version = "1", features = ["derive"] }
serde_json = "1"
sha2 = "0.10"
tar = "0.4"
flate2 = "1"
digest = "0.10"
hex = "0.4"
thiserror = "2"
chrono = { version = "0.4", features = ["serde"] }

# crates/oci-registry/Cargo.toml
[dependencies]
oci-image = { path = "../oci-image" }
kaniko-creds = { path = "../kaniko-creds" }
reqwest = { version = "0.12", features = ["json", "rustls-tls"] }
tokio = { version = "1", features = ["full"] }
serde_json = "1"
thiserror = "2"
tracing = "0.1"

# crates/kaniko-snapshot/Cargo.toml
[dependencies]
oci-image = { path = "../oci-image" }
nix = { version = "0.29", features = ["fs", "ioctl"] }
walkdir = "2"
sha2 = "0.10"
tar = "0.4"
thiserror = "2"
tracing = "0.1"

# crates/kaniko-core/Cargo.toml
[dependencies]
oci-image = { path = "../oci-image" }
oci-registry = { path = "../oci-registry" }
dockerfile-parser = { path = "../dockerfile-parser" }
kaniko-snapshot = { path = "../kaniko-snapshot" }
kaniko-cache = { path = "../kaniko-cache" }
kaniko-creds = { path = "../kaniko-creds" }
tokio = { version = "1", features = ["full"] }
async-trait = "0.1"
thiserror = "2"
tracing = "0.1"

# crates/kaniko-cli/Cargo.toml
[dependencies]
kaniko-core = { path = "../kaniko-core" }
clap = { version = "4", features = ["derive"] }
tracing = "0.1"
tracing-subscriber = "0.3"
tokio = { version = "1", features = ["full"] }
```

### 14.2 构建配置

```toml
# Cargo.toml (workspace 根)
[workspace]
members = [
    "crates/*",
]
resolver = "2"

[profile.release]
opt-level = "z"      # 最小体积
lto = true           # Link-Time Optimization
codegen-units = 1    # 更好的优化
strip = true         # 去除符号信息
panic = "abort"      # 减小运行时

[profile.release.package."*"]
opt-level = "z"
```

---

## 15. 总结

本方案选择**路径 C（自建 OCI 镜像构建库）**，以 `oci-image` crate 为核心枢纽，通过 8 个 crate 组成的 workspace 实现 kaniko 的完整功能。关键决策：

1. **`oci-image` 是成败关键**：投入 35 人天（占总工时 19%），必须优先交付
2. **渐进式三阶段迁移**：8 周 MVP → 6 周功能对齐 → 4 周生产化
3. **异步架构**：全链路 tokio 异步，RUN 命令异步进程执行，缓存推送并行
4. **兼容性优先**：通过兼容性测试套件确保与 Go 版本行为一致
5. **体积目标**：< 10MB 未经压缩，3-5MB UPX 压缩后