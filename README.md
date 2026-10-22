# 云端多账号（Cloud Multi-Account）

QwenPaw 云端账号一体化插件（Windows/Linux 双端）：账号密码登录 · 会话续期 · 失效自动重登 · 多智能体会话 · 玻璃质感面板。

## ✨ 功能 (Features)

- **账号密码登录**：平台账号直接登录，token 自动保存
- **会话自动续期**：token 临近过期自动刷新，请求失败自动恢复，无需手动干预
- **失效自动重登**：token 失效自动用记住的密码重登
- **多智能体会话**：账号内多个智能体的历史对话按标签切换查看
- **多账号管理**：一个面板管理多个云端账号，重复登录自动合并
- **实例管理**：打开云端实例 / 重启实例 / 删除账号
- **桥自动看护**：本地桥异常自动恢复，崩溃有日志可查
- **毫秒级 Go 桥**：零外部依赖，Windows/Linux 自动识别
- **玻璃质感面板**：总览条 + 账号卡片网格 + 会话/详情明细（响应式）

## 🔧 兼容性 (Compatibility)

| 项目 | 支持 |
|---|---|
| QwenPaw | ≥ 1.0.0（推荐 1.x 最新） |
| Windows | x64（cloud_bridge_win.exe） |
| Linux | x64（cloud_bridge_linux） |
| 云端 QwenPaw 实例 | 已开通（platform.agentscope.io） |

## 📝 版本 (Version)

- **当前版本**：1.7.0
- 更新记录：
  - 1.7.0：新 UI（总览/卡片网格/会话明细）+ 账号去重 + token 有效期展示 + 空实例标注
  - 1.6.0：纯 Go 化（移除 Python 依赖）+ 多智能体会话修复
  - 1.5.0：token 预刷新 + 自动重登修复 + 桥看护

## 🚀 安装 (Install)

```
qwenpaw plugin install cloud-multi-account-v1.7.0.zip
# 或从插件市场（platform.agentscope.io）一键安装
```

安装后重启 QwenPaw → 侧边栏「设置 → 云端账号」→ 登录平台账号即可使用。

## 📖 使用 (Usage)

1. 打开「云端账号」面板
2. 「＋ 添加账号」→ 账号/密码/账号名 → 登录
3. 卡片显示：状态徽章 · 有效期 · token 剩余进度条
4. 点击卡片展开：💬 会话（按智能体切换）/ ℹ️ 详情
5. ⋮ 菜单：重启实例 / 重新登录 / 删除账号

## 📁 数据与日志 (Data & Logs)

- 账号数据：插件目录 `data/cloud_oauth_<name>.json`
- 插件日志：`logs/cloud_bridge.log`、`logs/cloud_bridge_go.log`

## 🧩 工作原理 (How it works)

```
插件启动本地桥（bridge/）→ 桥代理云端实例 API
前端面板 ↔ 本地桥 ↔ 云端实例
```

## 🌐 社区 (Community)

- 作者：User
- 反馈/问题：可通过 QwenPaw 插件市场讨论区反馈

## 📄 License

MIT License（见 LICENSE）
