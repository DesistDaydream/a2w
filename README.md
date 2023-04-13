# Alertmanager to WeCom bot

## templates

### `message.tmpl`

在告警规则定义中，必须包含：

- `labels.level`：告警规则等级。
- `annotations.current`：当前状态的表达式结果值，可以通过 `{{ $value }}` 获取。
- `annotations.labels`：可以定位到该告警实例的标签列表。
