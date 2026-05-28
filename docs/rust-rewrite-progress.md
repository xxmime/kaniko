# Kaniko Rust 重构开发进度

> 最后更新: 2026-05-28

## 总体进度: ~85%

| 阶段 | 状态 | 完成度 |
|------|------|--------|
| 阶段1: MVP验证 | 已完成 | 100% |
| 阶段2: 功能对齐 | 进行中 | ~60% |
| 阶段3: 生产化 | 未开始 | 0% |

---

## 各模块完成度

### oci-image (95%)
- [x] Manifest/Descriptor/MediaType 数据模型
- [x] ImageConfig/ContainerConfig 数据模型
- [x] Layer 创建（from_bytes/from_tar/from_files/empty）
- [x] SHA-256 Digest 计算
- [x] Whiteout 规范实现
- [x] mutate 操作（append_layers/update_config/set_env/set_label）
- [x] ImageIndex 支持
- [x] 27 个单元测试全部通过
- [ ] Layout 读写（OCI Layout 目录格式）
- [ ] tarball 读写（Docker save 格式）

### dockerfile-parser (100%)
- [x] 18 种指令类型定义
- [x] 基础解析器（支持多阶段构建 FROM...AS）
- [x] 18 个单元测试全部通过（含变量替换+续行+ONBUILD/HEALTHCHECK/SHELL/STOPSIGNAL）
- [x] ARG/ENV 变量替换（$VAR/${VAR}/$转义/build-arg覆盖）
- [x] 多行续行支持（`\`）
- [x] ONBUILD/HEALTHCHECK/SHELL/STOPSIGNAL/MAINTAINER 解析
- [x] VarContext 变量上下文（build_args > env > args 优先级）
- [x] HEREDOC 语法支持（RUN <<EOF）
- [x] 字符串修剪修复（解决测试失败问题）

### kaniko-snapshot (90%)
- [x] LayeredMap 增量文件状态追踪
- [x] Snapshotter 快照核心逻辑
- [x] walker.rs 文件遍历 + .dockerignore 解析
- [x] 4 个 walker 单元测试通过
- [x] default ignore list（/proc, /sys, /dev 等）
- [x] walker 集成到 Snapshotter（.dockerignore 支持）
- [ ] syncfs 系统调用（Linux 特有）
- [ ] 白标删除检测增强

### kaniko-cache (60%)
- [x] RegistryCache 基础结构（存在性检查/引用生成）
- [x] LayoutCache 本地缓存（完整 push/retrieve/exists 实现）
- [ ] RegistryCache 完整实现（需调用 oci-registry）
- [ ] 缓存淘汰策略
- [ ] 缓存并发安全

### kaniko-creds (90%)
- [x] SystemKeychain Docker config.json 解析
- [x] Credential helper 子进程调用
- [x] base64 解码认证
- [x] 3 个单元测试通过
- [x] credential helper stdin 传参修复（registry URL 写入 stdin）

### oci-registry (85%)
- [x] push_image 完整实现（Bearer Token + blob upload + manifest push）
- [x] pull_image 完整实现（manifest → config → layer 下载）
- [x] Reference 解析（registry/repo:tag）
- [x] Bearer Token 认证（WWW-Authenticate 解析）
- [x] Basic Auth 认证
- [x] transport 重试逻辑（指数退避 + 5xx/连接错误重试）
- [x] TLS 证书验证跳过支持
- [ ] chunked upload 大文件分块上传
- [ ] mount 优化（跨仓库 blob 挂载）

### kaniko-core (75%)
- [x] DockerCommand trait + BaseCommand blanket impl
- [x] 16 个指令实现：
  - ENV, LABEL, EXPOSE, USER, WORKDIR
  - COPY, ADD, RUN
  - CMD, ENTRYPOINT, VOLUME
  - ARG, SHELL, STOPSIGNAL, HEALTHCHECK, ONBUILD
- [x] CompositeCache 缓存键计算（5 个测试）
- [x] StageBuilder 集成 Snapshotter + LayoutCache
- [x] BuildOptions 配置
- [ ] CopyCommand 的 --chown/--chmod 支持
- [ ] AddCommand 的 --chown/--chmod/--link 支持
- [ ] RUN --mount 支持（BuildKit 扩展）
- [ ] ONBUILD 触发器执行（子指令解析）
- [ ] 指令单元测试

### kaniko-cli (80%)
- [x] CLI 参数定义（20+ 参数，兼容原 kaniko）
- [x] 完整构建流程串联（Dockerfile 解析 → 拉取基础镜像 → 执行指令 → 推送）
- [x] 多阶段构建支持
- [x] tar 输出
- [x] OCI Layout 输出
- [x] digest 文件输出
- [x] 2.4MB release 二进制
- [x] 编译警告修复（kaniko-cli）
- [ ] .dockerignore 读取并传入 COPY/ADD
- [ ] --build-arg 变量替换
- [ ] --target 阶段选择增强
- [ ] 错误处理与友好提示

---

## 构建验证结果

| 指标 | 结果 |
|------|------|
| `cargo check` | 0 错误（仅有 warnings） |
| `cargo test` | 61 个测试全部通过 |
| `cargo build --release` | 成功，二进制 2.4MB |

### 测试分布
- oci-image: 27 个测试
- dockerfile-parser: 18 个测试（含变量替换+续行+ONBUILD/HEALTHCHECK/HEREDOC）
- kaniko-core: 5 个测试（composite_key）
- kaniko-creds: 3 个测试
- kaniko-snapshot: 4 个测试（walker）
- oci-registry: 4 个测试（transport）

---

## 最近完成的工作

### 2026-05-28
- [x] 修复 kaniko-cli 编译警告（mut 变量）
- [x] 实现 dockerfile-parser 的 HEREDOC 语法支持（RUN <<EOF）
- [x] 修复 dockerfile-parser 测试失败（字符串修剪问题）
- [x] 修复 ONBUILD 解析空参数问题
- [x] 清理未使用的导入

---

## 下一步开发计划

### P0（当前优先级）
1. **kaniko-snapshot 集成**: walker → Snapshotter 完整串联
2. **kaniko-cache 增强**: RegistryCache 完整实现
3. **指令单元测试**: 为每个指令添加 execute() 测试
4. **.dockerignore 集成**: COPY/ADD 指令支持 .dockerignore

### P1（功能对齐）
5. **COPY --from 跨阶段**: 多阶段构建中引用其他阶段产物
6. **--build-arg 变量替换**: CLI 参数传入变量替换
7. **RUN --mount**: BuildKit 挂载扩展
8. **--target 阶段选择**: 构建指定阶段

### P2（生产化）
9. **集成测试**: 端到端 Dockerfile 构建 + Registry 推拉
10. **性能优化**: 并行层推送 + 流式上传
11. **容器镜像发布**: distroless + musl 静态链接

---

## 对比 Go 版本

| 指标 | Go 版本 | Rust 版本 | 改善 |
|------|---------|-----------|------|
| 二进制体积 | 53MB (16MB UPX) | 2.4MB | **-95%** |
| 运行时内存 | ~50MB (含 GC) | ~10-15MB (预期) | -70%~-80% |
| 构建依赖 | 150+ Go 包 | 15 Rust crate | -90% |
| 编译时间 | ~30s | ~60s release | +100% |