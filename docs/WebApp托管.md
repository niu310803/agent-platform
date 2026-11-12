# WebApp 托管退役说明

Platform 的 Compose 多应用宿主已移除。旧实现与历史验收记录保留在 Git 历史中，不再作为当前能力或后续域名部署计划。

- Desktop 继续管理多个 WebApp，共享模式使用主 Platform，独立模式使用应用专属 Platform 和环境。
- 普通 Platform 独立部署提供智能体服务，对接 agent-webclient。
- `--webapp-host=compose` 和 `runtime.yml` 的 `webapps:` 节不再接受，包括 `enabled: false`；删除旧节后按普通 Platform 启动。
- 在线 ZIP 上传、预检、安装、更新、卸载、应用列表和 operation 管理接口已删除。安装及前后端生命周期归外部宿主。
- 现有单实例适配见 [WebApp 独立实例](WebApp独立实例.md)，离线 `webapp check/pack` 保留。
- 不自动读取、迁移或删除旧 `webapps/` 和 `webapp-data/<id>/`。旧代码与授权目录继续受普通 Agent 工具保护，数据处理须由部署方明确安排。
