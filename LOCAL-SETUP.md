# WeKnora 本地开发/部署说明

## 远端与分支约定

| 名称 | 用途 |
|---|---|
| `origin` | 你的 fork（可 push） |
| `upstream` | 官方 `Tencent/WeKnora`（只拉取） |
| `main` | 跟踪 `upstream/main`，不放本地部署定制 |
| `local/weknora-*` | 本机 override、部署调优、实验性修复 |

推荐流程：

```bash
# 首次 fork 后
 git remote add upstream https://github.com/Tencent/WeKnora.git

# 更新上游
 git switch main
 git fetch upstream
 git reset --hard upstream/main

# 继续维护本地分支
 git switch local/weknora-config-backup
 git rebase main
```

如果要把本地定制备份到你自己的 GitHub：

```bash
git push -u origin local/weknora-config-backup
```

## fork 命名建议

| 项 | 建议 |
|---|---|
| fork 仓库名 | 直接保留 `WeKnora` |
| 原因 | 与上游一致最直观，脚本/文档/远端映射都更简单 |
| 差异承载 | 用分支名区分，不靠改仓库名 |

## 本机 override 用法

| 文件 | 用途 |
|---|---|
| `docker-compose.override.yml` | 本机端口、宿主路径、调试环境变量 |
| `docker-compose.override.yml.example` | 可复制模板 |
| `.env` | 密钥、服务地址、运行时开关 |
| `config/local/` / `config/overrides/` | 不进 git 的本机配置目录 |

复制模板：

```bash
cp docker-compose.override.yml.example docker-compose.override.yml
```

## Ollama 基线

| 项 | 值 |
|---|---|
| 监听地址 | `0.0.0.0:11434` |
| embedding 模型 | `quentinz/bge-large-zh-v1.5:f16` |
| 容器访问地址 | `http://host.docker.internal:11434` |

## Rerank 基线

| 项 | 建议 |
|---|---|
| `config/config.yaml` | `conversation.enable_rerank: false` |
| `.env` 的 `RERANK_*` | 留空 |
| 目标 | 避免外网 `jina` 超时，把检索延迟压到约 0.3s |

## 不要提交的内容

| 类型 | 示例 |
|---|---|
| 备份文件 | `*.bak` / `*.orig` |
| 本机覆盖 | `docker-compose.override.yml` |
| 日志/临时文件 | `logs/` / `tmp/` / `*.log` / `*.pid` |
| 密钥 | API key / token / 私有证书 |
