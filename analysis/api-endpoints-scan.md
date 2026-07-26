# QoderWork CN API 端点全扫描（客户端逆向 + 实测）

> 2026-07-27 · 来源：`/tmp/qw_extract`（CN 桌面客户端 main.js + agent-sdk worker）+
> 新 OAuth 账号（aliyun3675019832，dt-）实测。
> 域：`openapi.qoder.com.cn`（业务）/ `gateway.qoder.com.cn`（COSY 推理）/ `qoder.com.cn`（页面）

## 一、认证域（openapi.qoder.com.cn，Bearer 即可，无需 COSY）

### 登录/Token
| 端点 | 方法 | 用途 | 状态 |
|---|---|---|---|
| `/api/v1/deviceToken/poll` | GET | 设备授权轮询（nonce+verifier）→ dt-/drt- | ✅ 插件已实现 |
| `/api/v1/deviceToken/refresh` | POST | drt- 旋转刷新 → 新 dt-+drt- | ✅ 插件已实现 |
| `/api/v1/jobToken/exchange` | POST | PAT → jt-/jrt- | ✅ 插件已实现 |
| `/api/v1/jobToken/refresh` | POST | jrt- → 新 jt- | ✅ 插件已实现 |
| `/api/v1/userinfo` | GET | uid/name/user_type（dt-/jt- 均可） | ✅ 插件已实现 |

### 账号/积分
| 端点 | 方法 | 用途 | 状态 |
|---|---|---|---|
| `/api/v2/quota/usage` | GET | 积分总额/已用/剩余 | ✅ 插件已实现 |
| `/api/v2/user/plan` | GET | 计划层级（Pro Trial 等） | ✅ 插件已实现 |
| `/api/v3/user/status` | GET | 用户状态+白名单+plan（**新发现**，字段比 userinfo 全：quota/whitelistStatus/isQuotaExceeded/plan） | 🔵 未用，可替代 userinfo |
| `/api/v1/me/data-policy` | GET/POST? | 数据政策同意状态 | ⚪ 未验证 |

### 活动/奖励（sash）
| 端点 | 方法 | 用途 | 状态 |
|---|---|---|---|
| `/sash/api/v1/me/daily-check-in/status` | GET | 签到状态（CLAIMED_TODAY/CLAIMABLE） | ✅ 插件已实现 |
| `/sash/api/v1/me/daily-check-in/claim` | POST | 每日签到 +100 | ✅ 插件已实现 |
| `/sash/api/v1/me/pro-upgrade/eligibility` | GET | Pro 升级包资格（eligible/reason） | ✅ v0.2.6 已实现（登录自动领） |
| `/sash/api/v1/me/pro-upgrade/claim` | POST | Pro 升级包领取（一次性） | ✅ v0.2.6 已实现 |
| `/sash/api/v1/me/invitationCode` | GET | **邀请码**（code 字段，长期有效） | 🔵 新发现，未用——邀请拉新可能有奖励 |

### 杂项
| 端点 | 方法 | 用途 | 状态 |
|---|---|---|---|
| `/api/v1/share-links` / `/presign` | POST | 分享链接 | ⚪ 客户端功能，插件不需要 |
| `/api/v1/remote` / `/remote/code` | ? | 远程控制 | ⚪ 客户端功能 |
| `/api/v1/webSearch/oneSearch` / `/unifiedSearch` | ? | 联网搜索（agent-sdk） | ⚪ 推理增值服务，可关注 |
| `/api/v2/image/upload` | POST | 图片上传（多模态） | ⚪ 未来视觉模型可用 |
| `/api/v2/service/integrations/github/...` | ? | GitHub 集成 | ⚪ 客户端功能 |

## 二、推理域（gateway.qoder.com.cn，**必须 COSY 签名**）

| 端点 | 方法 | 用途 | 状态 |
|---|---|---|---|
| `/algo/api/v2/service/pro/sse/agent_chat_generation` | POST | 对话推理（SSE 流式） | ✅ 插件已实现 |
| `/algo/api/v2/model/list` | GET | 动态模型清单 | ✅ 插件已实现 |
| `/algo/api/v2/activity` | GET | **模型活动**：各模型限免额度（activityId/modelName/limit/used/remaining/resetAt/tag/detailUrl，Global 区域 us/sg/jp） | 🔵 新发现——「哪些模型有限免活动」的查询端点，可在面板展示；**非可领取包** |
| `/algo/api/v1/ping` | GET | 健康检查 | ⚪ 可用于探活 |
| `/algo/api/v2/byok/check` / `/byok/config` | ? | BYOK 自定义 key | ⚪ 客户端功能 |
| `/algo/api/v2/service/voice/polish` | ? | 语音润色 | ⚪ 无关 |
| `/api/v2/service/ws/asr` | WS | 语音识别 | ⚪ 无关 |

## 三、关键结论

1. **`/algo/api/v2/activity` 是「模型限免活动查询」**（如某模型每日免费 N 次），
   不是可领取的积分包——但信息有价值：面板可展示「当前哪些模型有限免」。
   需要 COSY 签名调用（同 model/list）。
2. **可领取的活动包目前只有两个**：每日签到（+100）+ Pro 升级包（一次性）。
   v0.2.6 已实现登录时自动判断 eligibility 并领取。
3. **邀请码**（`/sash/api/v1/me/invitationCode`）长期有效，邀请拉新可能有
   奖励机制——客户端未见到对应 claim 端点，奖励可能在被邀请人侧自动生效。
4. `/api/v3/user/status` 字段比 userinfo 全（含 whitelistStatus/isQuotaExceeded/plan），
   未来做账号状态展示可换用。
