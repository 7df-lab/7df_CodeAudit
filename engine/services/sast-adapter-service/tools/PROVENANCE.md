# tools/opengrep — sast-adapter 镜像烘焙工具来源

- 文件: `opengrep`（OpenGrep CLI，版本 **1.29.0**，`opengrep --version` 实测）
- 来源: OpenGrep 官方发布（semgrep 开源分叉，ADR-158 引入替代 semgrep；
  同 YAML 规则/同 JSON 输出，默认导出 dataflow_trace 变量级污点链路）
- 校验: `opengrep.sha256`（sha256sum）
- 形态: ELF x86-64，动态链接，**仅依赖 glibc libc.so.6（≥2.14）**——运行时镜像
  须为 glibc 系（debian-slim），不可用 alpine/musl
- 刷新: 官方 release 下载新版本二进制覆盖本文件并重算 sha256（构建机无 GitHub
  出口，须在宿主机下载后 vendor 进树，先例 CD/pentest-artifacts）
- 消费方: `services/sast-adapter-service/Dockerfile`（COPY 至 /usr/local/bin/opengrep）
- 目录名说明: 用 `tools/` 不用 `vendor/`——Go 模块根下 vendor/ 有特殊语义（-mod=vendor），会破坏容器内 go build
